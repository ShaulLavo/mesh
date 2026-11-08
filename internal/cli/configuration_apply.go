package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/update"
)

type feedConfigFile struct {
	dir      *os.File
	path     string
	contents []byte
	value    update.FeedValue
}

func (f *feedConfigFile) Value() update.FeedValue { return f.value }
func (f *feedConfigFile) Save(value update.FeedValue) error {
	next, err := replaceFeedValue(f.contents, value)
	if err != nil {
		return err
	}
	if len(next) > clientConfigLimit {
		return errors.New("host config exceeds 2 MiB")
	}
	if _, err = validatedFeedConfig(next, f.path); err != nil {
		return err
	}
	if err = publishClientConfig(f.dir, next, syncClientConfig); err != nil {
		return err
	}
	f.contents, f.value = next, value
	return nil
}
func (f *feedConfigFile) Settle() error {
	if err := syncClientConfig(f.dir); err != nil {
		return err
	}
	for path := filepath.Dir(f.path); ; path = filepath.Dir(path) {
		root, err := os.OpenRoot(path)
		if err != nil {
			return fmt.Errorf("open configuration parent: %w", err)
		}
		dir, err := root.Open(".")
		_ = root.Close()
		if err != nil {
			return fmt.Errorf("settle feed configuration parent: %w", err)
		}
		err = dir.Sync()
		closeErr := dir.Close()
		if err != nil || closeErr != nil {
			return fmt.Errorf("settle feed configuration parent: %w", errors.Join(err, closeErr))
		}
		if path == filepath.Dir(path) {
			return nil
		}
	}
}
func withFeedConfiguration(ctx context.Context, operation func(update.FeedFile) error) error {
	return withClientConfigLock(ctx, true, func(dir *os.File, path string) error {
		contents, err := readClientConfig(dir)
		if errors.Is(err, os.ErrNotExist) {
			contents, err = []byte(`{"version":1,"hosts":[]}`), nil
		}
		if err != nil {
			return err
		}
		value, err := validatedFeedConfig(contents, path)
		if err != nil {
			return &configurationDocumentError{cause: err}
		}
		return operation(&feedConfigFile{dir, path, contents, value})
	})
}
func validatedFeedConfig(contents []byte, path string) (update.FeedValue, error) {
	if err := checkConfigJSON(json.NewDecoder(bytes.NewReader(contents)), 0); err != nil {
		return update.FeedValue{}, err
	}
	config, err := parseHostConfig(contents, path)
	if err != nil {
		return update.FeedValue{}, err
	}
	if config.Dashboard != nil && config.Dashboard.Theme != "" {
		if err := ValidateDashboardTheme(config.Dashboard.Theme); err != nil {
			return update.FeedValue{}, err
		}
	}
	fields, err := configurationFields(contents)
	if err != nil {
		return update.FeedValue{}, err
	}
	dashboard, ok := fields["dashboard"]
	if !ok || bytes.Equal(bytes.TrimSpace(contents[dashboard.start:dashboard.end]), []byte("null")) {
		return update.FeedValue{}, nil
	}
	nested, err := configurationFields(contents[dashboard.start:dashboard.end])
	if err != nil {
		return update.FeedValue{}, err
	}
	_, present := nested["usageFeedURL"]
	value := update.FeedValue{Present: present}
	if config.Dashboard != nil {
		value.URL = config.Dashboard.UsageFeedURL
	}
	return value, nil
}

type configurationSpan struct{ start, end int }

func configurationFields(contents []byte) (map[string]configurationSpan, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("configuration: JSON object required")
	}
	result := make(map[string]configurationSpan)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("configuration field: %w", err)
		}
		start := int(decoder.InputOffset())
		for start < len(contents) && (contents[start] == ':' || bytes.ContainsRune([]byte(" \t\r\n"), rune(contents[start]))) {
			start++
		}
		var raw json.RawMessage
		if err = decoder.Decode(&raw); err != nil {
			return nil, fmt.Errorf("configuration value: %w", err)
		}
		result[key.(string)] = configurationSpan{start, int(decoder.InputOffset())}
	}
	if _, err = decoder.Token(); err != nil {
		return nil, fmt.Errorf("configuration object: %w", err)
	}
	return result, nil
}
func replaceFeedValue(contents []byte, value update.FeedValue) ([]byte, error) {
	if !value.Present {
		return nil, errors.New("configuration delivery requires an explicit feed value")
	}
	fields, err := configurationFields(contents)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value.URL)
	if err != nil {
		return nil, fmt.Errorf("encode usage feed: %w", err)
	}
	dashboard, exists := fields["dashboard"]
	if !exists {
		return insertConfigurationField(contents, fields, "dashboard", append(append([]byte(`{"usageFeedURL":`), encoded...), '}')), nil
	}
	raw := contents[dashboard.start:dashboard.end]
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return spliceConfiguration(contents, dashboard, append(append([]byte(`{"usageFeedURL":`), encoded...), '}')), nil
	}
	nested, err := configurationFields(raw)
	if err != nil {
		return nil, err
	}
	if span, found := nested["usageFeedURL"]; found {
		return spliceConfiguration(contents, configurationSpan{dashboard.start + span.start, dashboard.start + span.end}, encoded), nil
	}
	return spliceConfiguration(contents, dashboard, insertConfigurationField(raw, nested, "usageFeedURL", encoded)), nil
}
func insertConfigurationField(contents []byte, fields map[string]configurationSpan, key string, value []byte) []byte {
	end := len(bytes.TrimRight(contents, " \t\r\n")) - 1
	field := []byte(`"` + key + `":`)
	if len(fields) != 0 {
		field = append([]byte{','}, field...)
	}
	field = append(field, value...)
	return spliceConfiguration(contents, configurationSpan{end, end}, field)
}
func spliceConfiguration(contents []byte, span configurationSpan, value []byte) []byte {
	result := make([]byte, 0, len(contents)-span.end+span.start+len(value))
	result = append(result, contents[:span.start]...)
	result = append(result, value...)
	return append(result, contents[span.end:]...)
}

func preserveExplicitFeedDisable(dir *os.File, config hostConfig, next []byte) ([]byte, error) {
	if config.Dashboard == nil || config.Dashboard.UsageFeedURL != "" {
		return next, nil
	}
	prior, err := readClientConfig(dir)
	if errors.Is(err, os.ErrNotExist) {
		return next, nil
	}
	if err != nil {
		return nil, err
	}
	fields, err := configurationFields(prior)
	if err != nil {
		return nil, err
	}
	dashboard, ok := fields["dashboard"]
	if !ok || bytes.Equal(bytes.TrimSpace(prior[dashboard.start:dashboard.end]), []byte("null")) {
		return next, nil
	}
	nested, err := configurationFields(prior[dashboard.start:dashboard.end])
	if err != nil {
		return nil, err
	}
	feed, present := nested["usageFeedURL"]
	if !present || !bytes.Equal(bytes.TrimSpace(prior[dashboard.start+feed.start:dashboard.start+feed.end]), []byte(`""`)) {
		return next, nil
	}
	return replaceFeedValue(next, update.FeedValue{Present: true})

}

type configurationDocumentError struct{ cause error }

func (e *configurationDocumentError) Error() string {
	return "recipient hosts.json is invalid; inspect this file locally before configuration delivery"
}
func (e *configurationDocumentError) Unwrap() error { return e.cause }

type domainConfigFile struct {
	file  *domainpolicy.DomainFile
	gate  *domainpolicy.CatalogGate
	hosts []string
}

func (f *domainConfigFile) Value() update.DomainValue {
	return update.DomainValue{Present: f.file.Present, Policy: f.file.Policy}
}
func (f *domainConfigFile) Prepare(digest string) (domainpolicy.CatalogToken, error) {
	if err := f.gate.Prepare(digest, f.hosts); err != nil {
		return domainpolicy.CatalogToken{}, fmt.Errorf("prepare domain generation: %w", err)
	}
	return f.gate.Policy.CatalogToken, nil
}
func (f *domainConfigFile) Save(value update.DomainValue) error {
	if err := f.file.Save(value.Policy); err != nil {
		return fmt.Errorf("save selected domain policy: %w", err)
	}
	return nil
}
func (f *domainConfigFile) Settle() error {
	if err := f.file.Settle(); err != nil {
		return fmt.Errorf("settle domain policy: %w", err)
	}
	return nil
}
func withDomainConfiguration(ctx context.Context, operation func(update.DomainFile) error) error {
	state, err := paths.StateDirPath()
	if err != nil {
		return fmt.Errorf("locate domain generation state: %w", err)
	}
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	ids := make([]string, len(hosts))
	for i, host := range hosts {
		ids[i] = host.ID
	}
	err = domainpolicy.WithCatalogGate(ctx, state, func(gate *domainpolicy.CatalogGate) error {
		return domainpolicy.WithDomainFile(ctx, filepath.Join(filepath.Dir(path), "domains.json"), func(file *domainpolicy.DomainFile) error {
			return operation(&domainConfigFile{file: file, gate: gate, hosts: ids})
		})
	})
	if err != nil {
		return fmt.Errorf("domain configuration: %w", err)
	}
	return nil
}
