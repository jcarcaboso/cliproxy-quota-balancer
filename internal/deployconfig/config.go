package deployconfig

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Merge updates only this plugin's settings and the host plugin discovery settings.
func Merge(config, policy []byte) ([]byte, error) {
	var root, settings yaml.Node
	if errDecode := yaml.Unmarshal(config, &root); errDecode != nil {
		return nil, fmt.Errorf("parse host configuration: %w", errDecode)
	}
	if errDecode := yaml.Unmarshal(policy, &settings); errDecode != nil {
		return nil, fmt.Errorf("parse plugin policy: %w", errDecode)
	}
	if len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode ||
		len(settings.Content) != 1 || settings.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("host configuration and plugin policy must be YAML mappings")
	}
	plugins, errPlugins := mapping(root.Content[0], "plugins")
	if errPlugins != nil {
		return nil, errPlugins
	}
	configs, errConfigs := mapping(plugins, "configs")
	if errConfigs != nil {
		return nil, errConfigs
	}
	set(plugins, "enabled", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
	set(plugins, "dir", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "/CLIProxyAPI/plugins"})
	set(configs, "quota-balancer", settings.Content[0])
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if errEncode := encoder.Encode(&root); errEncode != nil {
		return nil, errEncode
	}
	if errClose := encoder.Close(); errClose != nil {
		return nil, errClose
	}
	return output.Bytes(), nil
}

func mapping(parent *yaml.Node, key string) (*yaml.Node, error) {
	for i := 0; i < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			node := parent.Content[i+1]
			if node.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("%s must be a YAML mapping", key)
			}
			return node, nil
		}
	}
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	set(parent, key, node)
	return node, nil
}

func set(parent *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			parent.Content[i+1] = value
			return
		}
	}
	parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

// Apply preserves a first-run backup and atomically updates the persisted config.
func Apply(path, policyPath string) error {
	config, errRead := os.ReadFile(path)
	if errRead != nil {
		return fmt.Errorf("read host configuration: %w", errRead)
	}
	policy, errPolicy := os.ReadFile(policyPath)
	if errPolicy != nil {
		return fmt.Errorf("read plugin policy: %w", errPolicy)
	}
	updated, errMerge := Merge(config, policy)
	if errMerge != nil {
		return errMerge
	}
	if bytes.Equal(config, updated) {
		return nil
	}
	backup, errBackup := os.OpenFile(path+".before-quota-balancer", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errBackup != nil && !os.IsExist(errBackup) {
		return fmt.Errorf("create configuration backup: %w", errBackup)
	}
	if errBackup == nil {
		_, errWrite := backup.Write(config)
		errClose := backup.Close()
		if errWrite != nil || errClose != nil {
			// A failed backup must not prevent a later attempt from making a complete copy.
			errRemove := os.Remove(backup.Name())
			return fmt.Errorf("write configuration backup: write=%v close=%v cleanup=%v", errWrite, errClose, errRemove)
		}
	}
	temp, errTemp := os.CreateTemp(filepath.Dir(path), ".quota-config-*")
	if errTemp != nil {
		return errTemp
	}
	if errMode := temp.Chmod(0600); errMode != nil {
		_ = temp.Close()
		_ = os.Remove(temp.Name())
		return errMode
	}
	_, errWrite := temp.Write(updated)
	errSync := temp.Sync()
	errClose := temp.Close()
	if errWrite != nil || errSync != nil || errClose != nil {
		_ = os.Remove(temp.Name())
		return fmt.Errorf("write configuration: write=%v sync=%v close=%v", errWrite, errSync, errClose)
	}
	if errRename := os.Rename(temp.Name(), path); errRename != nil {
		_ = os.Remove(temp.Name())
		return errRename
	}
	return nil
}
