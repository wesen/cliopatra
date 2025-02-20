package pkg

import (
	"context"
	"fmt"
	"github.com/go-go-golems/glazed/pkg/cmds/layers"
	"github.com/go-go-golems/glazed/pkg/cmds/parameters"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Parameter describes a cliopatra parameter, which can be either a flag or an argument.
// It does mirror the definition of parameters.ParameterDefinition, but here we only
// have a Value, and a Short description (which should actually describe which value we chose).
//
// The Flag makes it possible to override the flag used on the CLI, if necessary.
// The Raw field makes it possible to pass a raw string to override the value being rendered
// out. This is useful to for example test invalid value for flags.
type Parameter struct {
	Name    string                   `yaml:"name"`
	Flag    string                   `yaml:"flag,omitempty"`
	Short   string                   `yaml:"short"`
	Type    parameters.ParameterType `yaml:"type"`
	Value   interface{}              `yaml:"value"`
	Raw     string                   `yaml:"raw,omitempty"`
	NoValue bool                     `yaml:"noValue,omitempty"`
}

// NOTE(manuel, 2023-03-16) What about sandboxing the execution of the command, especially if it outputs files
// NOTE(manuel, 2023-03-16) It would be interesting to provide some more tests on the output (say, as shell scripts)
// NOTE(manuel, 2023-03-16) What about measuring profiling regression

func (p *Parameter) Clone() *Parameter {
	return &Parameter{
		Name:    p.Name,
		Flag:    p.Flag,
		Short:   p.Short,
		Type:    p.Type,
		Value:   p.Value,
		Raw:     p.Raw,
		NoValue: p.NoValue,
	}
}

// Program describes a program to be executed by cliopatra.
//
// This can be used for golden tests by providing the
type Program struct {
	Name        string   `yaml:"name"`
	Path        string   `yaml:"path,omitempty"`
	Verbs       []string `yaml:"verbs,omitempty"`
	Description string   `yaml:"description"`
	// Env makes it possible to specify environment variables to set manually
	Env map[string]string `yaml:"env,omitempty"`

	// TODO(manuel, 2023-03-16) Probably add RawFlags here, when we say quickly want to record a run.
	// Of course, if we are using Command, we could have that render a more precisely described
	// cliopatra file. But just capturing normal calls is nice too.
	RawFlags []string `yaml:"rawFlags,omitempty"`

	// These Flags will be passed to the CLI tool. This allows us to register
	// flags with a type to cobra itself, when exposing this command again.
	Flags []*Parameter `yaml:"flags,omitempty"`
	// Args is an ordered list of Parameters. The Flag field is ignored.
	Args []*Parameter `yaml:"args,omitempty"`
	// Stdin makes it possible to pass data into stdin. If empty, no data is passed.
	Stdin string `yaml:"stdin,omitempty"`

	// These fields are useful for golden testing.
	ExpectedStdout     string            `yaml:"expectedStdout,omitempty"`
	ExpectedError      string            `yaml:"expectedError,omitempty"`
	ExpectedStatusCode int               `yaml:"expectedStatusCode,omitempty"`
	ExpectedFiles      map[string]string `yaml:"expectedFiles,omitempty"`
}

func NewProgramFromYAML(s io.Reader) (*Program, error) {
	var program Program
	if err := yaml.NewDecoder(s).Decode(&program); err != nil {
		return nil, errors.Wrap(err, "could not decode program")
	}
	return &program, nil
}

func (p *Program) Clone() *Program {
	clone := *p

	clone.RawFlags = make([]string, len(p.RawFlags))
	copy(clone.RawFlags, p.RawFlags)
	clone.Flags = make([]*Parameter, len(p.Flags))
	for i, f := range p.Flags {
		clone.Flags[i] = f.Clone()
	}
	clone.Args = make([]*Parameter, len(p.Args))
	for i, a := range p.Args {
		clone.Args[i] = a.Clone()
	}
	clone.Env = make(map[string]string, len(p.Env))
	for k, v := range p.Env {
		clone.Env[k] = v
	}

	clone.ExpectedFiles = make(map[string]string, len(p.ExpectedFiles))
	for k, v := range p.ExpectedFiles {
		clone.ExpectedFiles[k] = v
	}

	return &clone
}

func (p *Program) SetFlagValue(name string, value interface{}) error {
	for _, f := range p.Flags {
		if f.Name == name {
			f.Value = value
			return nil
		}
	}

	return fmt.Errorf("could not find flag %s", name)
}

func (p *Program) SetFlagRaw(name string, raw string) error {
	for _, f := range p.Flags {
		if f.Name == name {
			f.Raw = raw
			return nil
		}
	}

	return fmt.Errorf("could not find flag %s", name)
}

func (p *Program) SetArgValue(name string, value interface{}) error {
	for _, a := range p.Args {
		if a.Name == name {
			a.Value = value
			return nil
		}
	}

	return fmt.Errorf("could not find arg %s", name)
}

func (p *Program) SetArgRaw(name string, raw string) error {
	for _, a := range p.Args {
		if a.Name == name {
			a.Raw = raw
			return nil
		}
	}

	return fmt.Errorf("could not find arg %s", name)
}

func (p *Program) AddRawFlag(raw ...string) {
	p.RawFlags = append(p.RawFlags, raw...)
}

func (p *Program) RunIntoWriter(
	ctx context.Context,
	parsedLayers map[string]*layers.ParsedParameterLayer,
	ps map[string]interface{},
	w io.Writer) error {
	var err error
	path := p.Path
	if path == "" {
		path, err = exec.LookPath(p.Name)
		if err != nil {
			return errors.Wrapf(err, "could not find executable %s", p.Name)
		}
	}

	args, err2 := p.ComputeArgs(ps)
	if err2 != nil {
		return err2
	}

	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{}
	// copy current environment
	cmd.Env = append(cmd.Env, os.Environ()...)
	for k, v := range p.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	if p.Stdin != "" {
		cmd.Stdin = strings.NewReader(p.Stdin)
	}
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return errors.Wrapf(err, "could not run %s", p.Name)
	}

	return nil
}

func (p *Program) ComputeArgs(ps map[string]interface{}) ([]string, error) {
	var err error

	args := []string{}

	args = append(args, p.Verbs...)

	// I'm not sure how useful this raw flags mixed with the other stuff is at all.
	// I don't think both together make much sense, maybe we should differentiate
	// at a higher level, so that it is either RawFlags, or all the rest
	args = append(args, p.RawFlags...)

	for _, flag := range p.Flags {
		flag_ := flag.Flag
		if flag_ == "" {
			flag_ = "--" + flag.Name
		}
		if flag.NoValue {
			args = append(args, flag_)
			continue
		}

		value, ok := ps[flag.Name]
		value_ := ""
		if !ok {
			value_ = flag.Raw
		} else {
			value_, err = parameters.RenderValue(flag.Type, value)
			if err != nil {
				return nil, errors.Wrapf(err, "could not render flag %s", flag.Name)
			}
		}

		if value_ == "" {
			value_, err = parameters.RenderValue(flag.Type, flag.Value)
			if err != nil {
				return nil, errors.Wrapf(err, "could not render flag %s", flag.Name)
			}
		}
		args = append(args, flag_)
		args = append(args, value_)
	}

	for _, arg := range p.Args {
		value, ok := ps[arg.Name]
		value_ := ""
		if !ok {
			value_ = arg.Raw
		} else {
			value_, err = parameters.RenderValue(arg.Type, value)
			if err != nil {
				return nil, errors.Wrapf(err, "could not render arg %s", arg.Name)
			}
		}

		if value_ == "" {
			value_, err = parameters.RenderValue(arg.Type, arg.Value)
			if err != nil {
				return nil, errors.Wrapf(err, "could not render arg %s", arg.Name)
			}
		}
		args = append(args, value_)
	}
	return args, nil
}

func LoadProgramsFromFS(f fs.FS, dir string) ([]*Program, error) {
	programs := []*Program{}

	entries, err := fs.ReadDir(f, dir)
	if err != nil {
		return nil, errors.Wrapf(err, "could not read dir %s", dir)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		fileName := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			programs_, err := LoadProgramsFromFS(f, fileName)
			if err != nil {
				return nil, errors.Wrapf(err, "could not load programs from dir %s", fileName)
			}
			programs = append(programs, programs_...)
			continue
		}

		if strings.HasSuffix(entry.Name(), ".yaml") ||
			strings.HasSuffix(entry.Name(), ".yml") {
			file, err := f.Open(fileName)
			if err != nil {
				return nil, errors.Wrapf(err, "could not open file %s", fileName)
			}

			defer func() {
				_ = file.Close()
			}()

			program, err := NewProgramFromYAML(file)
			if err != nil {
				return nil, errors.Wrapf(err, "could not load program from file %s", fileName)
			}

			programs = append(programs, program)
		}
	}

	return programs, nil
}

func LoadRepositories(repositories []string) (map[string]*Program, error) {
	programs := map[string]*Program{}

	for _, repository := range repositories {
		_, err := os.Stat(repository)
		if err != nil {
			return nil, errors.Wrapf(err, "could not stat repository %s", repository)
		}

		programs_, err := LoadProgramsFromFS(os.DirFS(repository), ".")
		if err != nil {
			return nil, errors.Wrapf(err, "could not load programs from repository %s", repository)
		}

		for _, program := range programs_ {
			if _, ok := programs[program.Name]; ok {
				return nil, fmt.Errorf("program %s already exists", program.Name)
			}
			programs[program.Name] = program
		}
	}
	return programs, nil
}
