package updatebootstrap

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/shaul/mesh/internal/updateinstall"
)

type ManagedMigration struct {
	Original      string                 `json:"original"`
	Managed       string                 `json:"managed"`
	Definition    string                 `json:"definition"`
	Before        string                 `json:"before"`
	After         string                 `json:"after"`
	Prior         ServiceSnapshot        `json:"prior"`
	Next          ServiceSnapshot        `json:"next"`
	Settings      updateinstall.Settings `json:"settings"`
	PriorSettings updateinstall.Settings `json:"priorSettings"`
	ImageExists   bool                   `json:"imageExists"`
	Applied       bool                   `json:"applied"`
	Prepared      bool                   `json:"prepared"`
	GateCleared   bool                   `json:"gateCleared"`
	Health        *updateinstall.Health  `json:"health,omitempty"`
}

// PlanManagedMigration computes the one executable change without writing any
// installation or service file. Raw service values never enter the plan.
func PlanManagedMigration(ctx context.Context, installation Installation) (*ManagedMigration, error) {
	if installation.PackagePath == "" {
		return nil, nil
	}
	invocation, err := inspectInvocation(ctx, installation.Observation.PID)
	if err != nil {
		return nil, err
	}
	managed := filepath.Join(invocation.Environment["HOME"], ".local", "bin", "mesh")
	if managed == installation.Observation.Executable {
		return nil, errors.New("managed migration path still names the package payload")
	}
	plan := &ManagedMigration{Original: installation.Observation.Executable, Managed: managed, Prior: installation.Service, Next: installation.Service, Settings: installation.Settings, PriorSettings: installation.Settings}
	plan.Next.Files = make(map[string]string, len(plan.Prior.Files)+1)
	for path, digest := range plan.Prior.Files {
		plan.Next.Files[path] = digest
	}
	content, err := managedServiceDefinition(ctx, installation, invocation, plan)
	if err != nil {
		return nil, err
	}
	plan.After = installationDigest(content)
	plan.Next.Files[plan.Definition] = plan.After
	plan.Next.Definition = serviceFilesDigest(plan.Next.Files)
	plan.Settings.Executable = managed
	info, err := os.Lstat(managed)
	if err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("managed destination is not a regular executable")
		}
		digest, err := imageDigest(managed)
		if err != nil || digest != installation.Observation.Health.Build.Digest {
			return nil, errors.Join(errors.New("managed destination already contains a different image"), err)
		}
		plan.ImageExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect managed migration destination: %w", err)
	}
	return plan, nil
}

func managedServiceDefinition(ctx context.Context, installation Installation, invocation processInvocation, plan *ManagedMigration) ([]byte, error) {
	if installation.Settings.Service.Kind == installationSystemd {
		plan.Definition = filepath.Join(invocation.Environment["HOME"], ".config", installationSystemd, "user", installation.Settings.Service.Name+".d", "90-mesh-managed.conf")
		plan.Before = "absent"
		if _, err := os.Lstat(plan.Definition); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.Join(errors.New("managed migration drop-in already exists"), err)
		}
		args := append([]string{plan.Managed}, invocation.Arguments[1:]...)
		quoted, err := quoteManagedArguments(args)
		if err != nil {
			return nil, err
		}
		return []byte("[Service]\nExecStart=\nExecStart=" + strings.Join(quoted, " ") + "\n"), nil
	}
	plan.Definition = installation.Settings.Service.ConfigPath
	plan.Before = installation.Service.Files[plan.Definition]
	command := exec.CommandContext(ctx, "plutil", "-convert", "xml1", "-o", "-")
	command.Args = append(command.Args, plan.Definition)
	data, err := inspectServiceCommand(ctx, command)
	if err != nil {
		return nil, err
	}
	return migrateLaunchAgent(data, invocation.Arguments, plan.Managed)
}

func quoteManagedArguments(arguments []string) ([]string, error) {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		if strings.ContainsAny(argument, "\x00\r\n") {
			return nil, errors.New("daemon argument cannot be represented in an executable-only service migration")
		}
		quoted[index] = managedSystemdQuote(argument)
	}
	return quoted, nil
}

func managedSystemdQuote(value string) string {
	return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%", "$", "$$").Replace(value) + "\""
}

type propertyListNode struct {
	XMLName xml.Name
	Attrs   []xml.Attr         `xml:",any,attr"`
	Text    string             `xml:",chardata"`
	Values  []propertyListNode `xml:",any"`
}

func migrateLaunchAgent(data []byte, arguments []string, managed string) ([]byte, error) {
	var document propertyListNode
	if err := xml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("decode launch agent for executable migration: %w", err)
	}
	if document.XMLName.Local != "plist" || len(document.Values) != 1 || document.Values[0].XMLName.Local != "dict" || len(arguments) < 2 {
		return nil, errors.New("launch agent has no supported property dictionary")
	}
	if err := migrateLaunchAgentDictionary(document.Values[0].Values, arguments, managed); err != nil {
		return nil, err
	}
	updated, err := xml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode executable-only launch agent migration: %w", err)
	}
	return append([]byte(xml.Header), updated...), nil
}

func migrateLaunchAgentDictionary(values []propertyListNode, arguments []string, managed string) error {
	if len(values)%2 != 0 {
		return errors.New("launch agent property dictionary is incomplete")
	}
	changed := false
	for index := 0; index < len(values); index += 2 {
		key, value := values[index], &values[index+1]
		if key.XMLName.Local != "key" {
			return errors.New("launch agent property is not keyed")
		}
		didChange, err := migrateLaunchAgentProperty(key.Text, value, arguments, managed)
		if err != nil {
			return err
		}
		changed = changed || didChange
	}
	if !changed {
		return errors.New("launch agent does not execute the observed Mesh image directly")
	}
	return nil
}

func migrateLaunchAgentProperty(key string, value *propertyListNode, arguments []string, managed string) (bool, error) {
	switch key {
	case "Program":
		if value.XMLName.Local != "string" || value.Text != arguments[0] {
			return false, errors.New("launch agent program differs from the observed executable")
		}
		value.Text = managed
		return true, nil
	case "ProgramArguments":
		if value.XMLName.Local != "array" || len(value.Values) != len(arguments) {
			return false, errors.New("launch agent arguments differ from the observed invocation")
		}
		var actual []string
		for _, entry := range value.Values {
			actual = append(actual, entry.Text)
		}
		if !reflect.DeepEqual(actual, arguments) {
			return false, errors.New("launch agent arguments differ from the observed invocation")
		}
		value.Values[0].Text = managed
		return true, nil
	default:
		return false, nil
	}
}

// ManagedMigrationContent recreates exactly the approved definition while the
// original process is still running. A changed invocation invalidates the plan.
func ManagedMigrationContent(ctx context.Context, installation Installation, expected ManagedMigration) ([]byte, error) {
	invocation, err := inspectInvocation(ctx, installation.Observation.PID)
	if err != nil {
		return nil, err
	}
	actual := expected
	content, err := managedServiceDefinition(ctx, installation, invocation, &actual)
	if err != nil {
		return nil, err
	}
	if installationDigest(content) != expected.After || actual.Definition != expected.Definition || actual.Before != expected.Before {
		return nil, errors.New("managed migration definition changed from its approved plan")
	}
	return content, nil
}

func VerifyManagedMigrationSource(ctx context.Context, plan ManagedMigration, digest string) (bool, error) {
	if err := verifyManagedMigrationFiles(plan); err != nil {
		return false, err
	}
	pid, err := migrationServicePID(ctx, plan.Settings.Service)
	if err != nil || pid == 0 {
		return false, err
	}
	image, err := processImage(ctx, pid)
	if err != nil {
		return true, err
	}
	actual, err := imageDigest(image.Path)
	if err != nil || actual != digest {
		return true, errors.Join(errors.New("migration manager process is not the approved old daemon image"), err)
	}
	invocation, err := inspectInvocation(ctx, pid)
	if err != nil {
		return true, err
	}
	args, environment := invocationSnapshot(invocation)
	if args != plan.Prior.Arguments || environment != plan.Prior.Environment {
		return true, errors.New("migration daemon arguments or environment changed")
	}
	return true, nil
}

func verifyManagedMigrationFiles(plan ManagedMigration) error {
	for path, expected := range plan.Prior.Files {
		if path == plan.Definition {
			continue
		}
		actual, err := serviceFileDigest(path)
		if err != nil || actual != expected {
			return errors.Join(errors.New("original daemon service changed outside managed migration"), err)
		}
	}
	definition, err := serviceFileDigest(plan.Definition)
	if errors.Is(err, os.ErrNotExist) {
		definition, err = "absent", nil
	}
	if err != nil || definition != plan.Before && definition != plan.After {
		return errors.Join(errors.New("migration service definition differs from its exact approval"), err)
	}
	return nil
}
