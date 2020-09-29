// nolint: gosec
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/crypto/ed25519"
	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/privval"
	e2e "github.com/tendermint/tendermint/test/e2e/pkg"
	"github.com/tendermint/tendermint/types"
)

const (
	AppAddressTCP  = "tcp://127.0.0.1:30000"
	AppAddressUNIX = "unix:///var/run/app.sock"

	PrivvalAddressTCP     = "tcp://0.0.0.0:27559"
	PrivvalAddressUNIX    = "unix:///var/run/privval.sock"
	PrivvalKeyFile        = "config/priv_validator_key.json"
	PrivvalStateFile      = "data/priv_validator_state.json"
	PrivvalDummyKeyFile   = "config/dummy_validator_key.json"
	PrivvalDummyStateFile = "data/dummy_validator_state.json"
)

// Setup sets up the testnet configuration.
func Setup(testnet *e2e.Testnet) error {
	logger.Info(fmt.Sprintf("Generating testnet files in %q", testnet.Dir))

	err := os.MkdirAll(testnet.Dir, os.ModePerm)
	if err != nil {
		return err
	}

	compose, err := MakeDockerCompose(testnet)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(filepath.Join(testnet.Dir, "docker-compose.yml"), compose, 0644)
	if err != nil {
		return err
	}

	genesis, err := MakeGenesis(testnet)
	if err != nil {
		return err
	}

	for _, node := range testnet.Nodes {
		nodeDir := filepath.Join(testnet.Dir, node.Name)
		dirs := []string{
			filepath.Join(nodeDir, "config"),
			filepath.Join(nodeDir, "data"),
			filepath.Join(nodeDir, "data", "app"),
		}
		for _, dir := range dirs {
			err := os.MkdirAll(dir, 0755)
			if err != nil {
				return err
			}
		}

		err = genesis.SaveAs(filepath.Join(nodeDir, "config", "genesis.json"))
		if err != nil {
			return err
		}

		cfg, err := MakeConfig(node)
		if err != nil {
			return err
		}
		config.WriteConfigFile(filepath.Join(nodeDir, "config", "config.toml"), cfg) // panics

		appCfg, err := MakeAppConfig(node)
		if err != nil {
			return err
		}
		err = ioutil.WriteFile(filepath.Join(nodeDir, "config", "app.toml"), appCfg, 0644)
		if err != nil {
			return err
		}

		err = (&p2p.NodeKey{PrivKey: node.Key}).SaveAs(filepath.Join(nodeDir, "config", "node_key.json"))
		if err != nil {
			return err
		}

		(privval.NewFilePV(node.Key,
			filepath.Join(nodeDir, PrivvalKeyFile),
			filepath.Join(nodeDir, PrivvalStateFile),
		)).Save()

		// Set up a dummy validator. Tendermint requires a file PV even when not used, so we
		// give it a dummy such that it will fail if it actually tries to use it.
		(privval.NewFilePV(ed25519.GenPrivKey(),
			filepath.Join(nodeDir, PrivvalDummyKeyFile),
			filepath.Join(nodeDir, PrivvalDummyStateFile),
		)).Save()
	}

	return nil
}

// MakeDockerCompose generates a Docker Compose config for a testnet.
func MakeDockerCompose(testnet *e2e.Testnet) ([]byte, error) {
	// Must use version 2 Docker Compose format, to support IPv6.
	tmpl, err := template.New("docker-compose").Parse(`version: '2.4'

networks:
  {{ .Name }}:
    driver: bridge
{{- if .IPv6 }}
    enable_ipv6: true
{{- end }}
    ipam:
      driver: default
      config:
      - subnet: {{ .IP }}

services:
{{- range .Nodes }}
  {{ .Name }}:
    container_name: {{ .Name }}
    image: tendermint/e2e-node
    init: true
    ports:
    - 26656
    - {{ if .ProxyPort }}{{ .ProxyPort }}:{{ end }}26657
    volumes:
    - ./{{ .Name }}:/tendermint
    networks:
      {{ $.Name }}:
        ipv{{ if $.IPv6 }}6{{ else }}4{{ end}}_address: {{ .IP }}

{{end}}`)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, testnet)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MakeGenesis generates a genesis document.
func MakeGenesis(testnet *e2e.Testnet) (types.GenesisDoc, error) {
	genesis := types.GenesisDoc{
		GenesisTime:     time.Now(),
		ChainID:         testnet.Name,
		ConsensusParams: types.DefaultConsensusParams(),
		InitialHeight:   testnet.InitialHeight,
	}
	for validator, power := range testnet.Validators {
		genesis.Validators = append(genesis.Validators, types.GenesisValidator{
			Name:    validator.Name,
			Address: validator.Key.PubKey().Address(),
			PubKey:  validator.Key.PubKey(),
			Power:   power,
		})
	}
	// The validator set will be sorted internally by Tendermint ranked by power,
	// but we sort it here as well so that all genesis files are identical.
	sort.Slice(genesis.Validators, func(i, j int) bool {
		return strings.Compare(genesis.Validators[i].Name, genesis.Validators[j].Name) == -1
	})
	if len(testnet.InitialState) > 0 {
		appState, err := json.Marshal(testnet.InitialState)
		if err != nil {
			return genesis, err
		}
		genesis.AppState = appState
	}
	return genesis, genesis.ValidateAndComplete()
}

// MakeConfig generates a Tendermint config for a node.
func MakeConfig(node *e2e.Node) (*config.Config, error) {
	cfg := config.DefaultConfig()
	cfg.Moniker = node.Name
	cfg.ProxyApp = AppAddressTCP
	cfg.RPC.ListenAddress = "tcp://0.0.0.0:26657"
	cfg.P2P.ExternalAddress = fmt.Sprintf("tcp://%v", node.AddressP2P(false))
	cfg.P2P.AddrBookStrict = false
	cfg.DBBackend = node.Database
	cfg.StateSync.DiscoveryTime = 5 * time.Second

	switch node.ABCIProtocol {
	case e2e.ProtocolUNIX:
		cfg.ProxyApp = AppAddressUNIX
	case e2e.ProtocolTCP:
		cfg.ProxyApp = AppAddressTCP
	case e2e.ProtocolGRPC:
		cfg.ProxyApp = AppAddressTCP
		cfg.ABCI = "grpc"
	default:
		return nil, fmt.Errorf("unexpected ABCI protocol setting %q", node.ABCIProtocol)
	}

	// Tendermint errors if it does not have a privval key set up, regardless of whether
	// it's actually needed (e.g. for remote KMS or non-validators). We set up a dummy
	// key here by default, and use the real key for actual validators that should use
	// the file privval.
	cfg.PrivValidatorListenAddr = ""
	cfg.PrivValidatorKey = PrivvalDummyKeyFile
	cfg.PrivValidatorState = PrivvalDummyStateFile

	switch node.Mode {
	case e2e.ModeValidator:
		switch node.PrivvalProtocol {
		case e2e.ProtocolFile:
			cfg.PrivValidatorKey = PrivvalKeyFile
			cfg.PrivValidatorState = PrivvalStateFile
		case e2e.ProtocolUNIX:
			cfg.PrivValidatorListenAddr = PrivvalAddressUNIX
		case e2e.ProtocolTCP:
			cfg.PrivValidatorListenAddr = PrivvalAddressTCP
		default:
			return nil, fmt.Errorf("invalid privval protocol setting %q", node.PrivvalProtocol)
		}
	case e2e.ModeSeed:
		cfg.P2P.SeedMode = true
		cfg.P2P.PexReactor = true
	case e2e.ModeFull:
		// Don't need to do anything, since we're using a dummy privval key by default.
	default:
		return nil, fmt.Errorf("unexpected mode %q", node.Mode)
	}

	if node.FastSync == "" {
		cfg.FastSyncMode = false
	} else {
		cfg.FastSync.Version = node.FastSync
	}

	if node.StateSync {
		cfg.StateSync.Enable = true
		cfg.StateSync.RPCServers = []string{}
		for _, peer := range node.Testnet.ArchiveNodes() {
			if peer.Name == node.Name {
				continue
			}
			cfg.StateSync.RPCServers = append(cfg.StateSync.RPCServers, peer.AddressRPC())
		}
		if len(cfg.StateSync.RPCServers) < 2 {
			return nil, errors.New("unable to find 2 suitable state sync RPC servers")
		}
	}

	cfg.P2P.Seeds = ""
	for _, seed := range node.Seeds {
		if len(cfg.P2P.Seeds) > 0 {
			cfg.P2P.Seeds += ","
		}
		cfg.P2P.Seeds += seed.AddressP2P(true)
	}
	cfg.P2P.PersistentPeers = ""
	for _, peer := range node.PersistentPeers {
		if len(cfg.P2P.PersistentPeers) > 0 {
			cfg.P2P.PersistentPeers += ","
		}
		cfg.P2P.PersistentPeers += peer.AddressP2P(true)
	}
	return cfg, nil
}

// MakeAppConfig generates an ABCI application config for a node.
func MakeAppConfig(node *e2e.Node) ([]byte, error) {
	cfg := map[string]interface{}{
		"chain_id":          node.Testnet.Name,
		"dir":               "data/app",
		"listen":            AppAddressUNIX,
		"grpc":              false,
		"persist_interval":  node.PersistInterval,
		"snapshot_interval": node.SnapshotInterval,
		"retain_blocks":     node.RetainBlocks,
	}
	switch node.ABCIProtocol {
	case e2e.ProtocolUNIX:
		cfg["listen"] = AppAddressUNIX
	case e2e.ProtocolTCP:
		cfg["listen"] = AppAddressTCP
	case "grpc":
		cfg["listen"] = AppAddressTCP
		cfg["grpc"] = true
	default:
		return nil, fmt.Errorf("unexpected ABCI protocol setting %q", node.ABCIProtocol)
	}
	switch node.PrivvalProtocol {
	case e2e.ProtocolFile:
	case e2e.ProtocolTCP:
		cfg["privval_server"] = PrivvalAddressTCP
		cfg["privval_key"] = PrivvalKeyFile
		cfg["privval_state"] = PrivvalStateFile
	case e2e.ProtocolUNIX:
		cfg["privval_server"] = PrivvalAddressUNIX
		cfg["privval_key"] = PrivvalKeyFile
		cfg["privval_state"] = PrivvalStateFile
	default:
		return nil, fmt.Errorf("unexpected privval protocol setting %q", node.PrivvalProtocol)
	}

	if len(node.Testnet.ValidatorUpdates) > 0 {
		validatorUpdates := map[string]map[string]int64{}
		for height, validators := range node.Testnet.ValidatorUpdates {
			updateVals := map[string]int64{}
			for node, power := range validators {
				updateVals[base64.StdEncoding.EncodeToString(node.Key.PubKey().Bytes())] = power
			}
			validatorUpdates[fmt.Sprintf("%v", height)] = updateVals
		}
		cfg["validator_update"] = validatorUpdates
	}

	var buf bytes.Buffer
	err := toml.NewEncoder(&buf).Encode(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to generate app config: %w", err)
	}
	return buf.Bytes(), nil
}

// UpdateConfigStateSync updates the state sync config for a node.
func UpdateConfigStateSync(node *e2e.Node, height int64, hash []byte) error {
	cfgPath := filepath.Join(node.Testnet.Dir, node.Name, "config", "config.toml")

	// FIXME Apparently there's no function to simply load a config file without
	// involving the entire Viper apparatus, so we'll just resort to regexps.
	bz, err := ioutil.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	bz = regexp.MustCompile(`(?m)^trust_height =.*`).ReplaceAll(bz, []byte(fmt.Sprintf(`trust_height = %v`, height)))
	bz = regexp.MustCompile(`(?m)^trust_hash =.*`).ReplaceAll(bz, []byte(fmt.Sprintf(`trust_hash = "%X"`, hash)))
	return ioutil.WriteFile(cfgPath, bz, 0644)
}