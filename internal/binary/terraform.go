// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package binary

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/opentffoundation/equivalence-testing/internal/files"

	"github.com/hashicorp/terraform-exec/tfexec"
)

// Command is a struct that instructs the framework how to execute a custom
// command. It covers the arguments that should be passed to the binary, and
// instructs whether the output should be captured and how it should be
// captured.
type Command struct {
	// The Name of the command to execute. This field is used for logging when
	// reporting which command might have failed.
	Name string `json:"name"`

	// A list of Arguments to pass to the binary, eg. `init`, `plan`,
	// `show -json`, etc.
	Arguments []string `json:"arguments"`

	// CaptureOutput should be set to true if we want to record the output of
	// this command and compare/copy it into the golden files.
	CaptureOutput bool `json:"capture_output"`

	// OutputFileName is the name of the file that the framework should write
	// the captured output into.
	//
	// This field is ignored if CaptureOutput is false.
	OutputFileName string `json:"output_file_name"`

	// HasJsonOutput tells the framework the output is going to be in JSON
	// format.
	//
	// This means that `IgnoreFields` section of the test specification can be
	// applied to the output of this command. It also provides a better diffing
	// output as JSON is easier to diff and display than raw strings.
	//
	// This field is ignored if CaptureOutput is false.
	HasJsonOutput bool `json:"has_json_output"`

	// StreamsJsonOutput tells the framework the output isn't going to arrive in
	// pure JSON but as a list of structured JSON statements. In this case the
	// framework will strip out any `\n` characters, put the output inside a
	// JSON list: `[`, `]`, and finally append the statements together with a
	// `,` character.
	//
	// This command basically turns the structured output into a JSON list that
	// can be handled by the rest of the framework. An example of this is the
	// output of an apply command: `$binary apply -json`.
	//
	// This field is ignored if CaptureOutput is false or if HasJsonOutput is
	// false.
	StreamsJsonOutput bool `json:"streams_json_output"`
}

// Binary is an interface that can execute a single equivalence test within a
// directory using the ExecuteTest method.
//
// We hold this in an interface, so we can mock it for testing purposes.
type Binary interface {
	// ExecuteTest executes a series of commands in order and returns the
	// output of the apply and plan steps, the state, and any additionally
	// requested files.
	ExecuteTest(directory string, includeFiles []string, commands ...Command) (map[string]*files.File, error)

	// Version returns the version of the underlying binary.
	Version() string
}

// New returns a Binary compatible struct that executes the tests using the
// selected binary provided in the argument.
func New(binaryName string) (Binary, error) {

	// First, sanity check binary actually points to a binary file.
	//
	// We do this by fetching the version using tfexec. tfexec tries to be
	// clever and look up cached provider versions as well, but we're not
	// interested in this, so we just set the working directory to be the
	// current directory and tfexec just won't find any binary or provider
	// files.
	//
	// Note, ideally we could actually just tfexec for everything. tfexec
	// doesn't (yet) support returning JSON files from the apply command so for
	// now we do the rest ourselves. Something to revisit in the future.
	tf, err := tfexec.NewTerraform(".", binaryName)
	if err != nil {
		return nil, err
	}

	version, _, err := tf.Version(context.Background(), true)
	if err != nil {
		return nil, err
	}

	return &binary{
		binary:  binaryName,
		version: version.String(),
	}, nil
}

type binary struct {
	binary  string
	version string
}

func (t *binary) Version() string {
	return t.version
}

func (t *binary) ExecuteTest(directory string, includeFiles []string, commands ...Command) (map[string]*files.File, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	if err := os.Chdir(directory); err != nil {
		return nil, err
	}
	defer os.Chdir(wd)

	savedFiles := map[string]*files.File{}
	if len(commands) == 0 {
		// We weren't given custom commands so let's run the default set of
		// commands.

		if err := t.init(); err != nil {
			return nil, err
		}
		if savedFiles["plan"], err = t.plan(); err != nil {
			return nil, err
		}
		if savedFiles["apply.json"], err = t.apply(); err != nil {
			return nil, err
		}
		if savedFiles["state"], err = t.showState(); err != nil {
			return nil, err
		}
		if savedFiles["state.json"], err = t.showJsonState(); err != nil {
			return nil, err
		}
		if savedFiles["plan.json"], err = t.showJsonPlan(); err != nil {
			return nil, err
		}
	} else {
		for _, command := range commands {
			output, err := t.command(command)
			if err != nil {
				return nil, err
			}

			if output != nil {
				savedFiles[command.OutputFileName] = output
			}
		}
	}

	for _, includeFile := range includeFiles {
		raw, err := os.ReadFile(includeFile)
		if err != nil {
			return nil, fmt.Errorf("could not read additional file (%s): %v", includeFile, err)
		}
		if savedFiles[includeFile], err = files.NewFile(includeFile, raw); err != nil {
			return nil, fmt.Errorf("could not unmarshal additional file (%s): %v", includeFile, err)
		}
	}

	return savedFiles, nil
}

func (t *binary) command(command Command) (*files.File, error) {
	capture, err := run(exec.Command(t.binary, command.Arguments...), command.Name)
	if err != nil {
		return nil, err
	}

	if !command.CaptureOutput {
		return nil, nil
	}

	if !command.HasJsonOutput {
		return files.NewRawFile(capture.ToString()), nil
	}

	var json interface{}
	if command.StreamsJsonOutput {
		if json, err = capture.ToJson(true); err != nil {
			return nil, err
		}
	} else {
		if json, err = capture.ToJson(false); err != nil {
			return nil, err
		}
	}
	return files.NewJsonFile(json), nil
}

func (t *binary) init() error {
	_, err := run(exec.Command(t.binary, "init"), "init")
	if err != nil {
		return err
	}
	return nil
}

func (t *binary) plan() (*files.File, error) {
	capture, err := run(exec.Command(t.binary, "plan", "-out=equivalence_test_plan", "-no-color"), "plan")
	if err != nil {
		return nil, err
	}
	return files.NewRawFile(capture.ToString()), nil
}

func (t *binary) apply() (*files.File, error) {
	capture, err := run(exec.Command(t.binary, "apply", "-json", "equivalence_test_plan"), "apply")
	if err != nil {
		return nil, err
	}

	json, err := capture.ToJson(true)
	if err != nil {
		return nil, err
	}
	return files.NewJsonFile(json), nil
}

func (t *binary) showState() (*files.File, error) {
	capture, err := run(exec.Command(t.binary, "show", "-no-color"), "show state")
	if err != nil {
		return nil, err
	}
	return files.NewRawFile(capture.ToString()), nil
}

func (t *binary) showJsonPlan() (*files.File, error) {
	capture, err := run(exec.Command(t.binary, "show", "-json", "equivalence_test_plan"), "show json plan")
	if err != nil {
		return nil, err
	}

	json, err := capture.ToJson(false)
	if err != nil {
		return nil, err
	}
	return files.NewJsonFile(json), nil
}

func (t *binary) showJsonState() (*files.File, error) {
	capture, err := run(exec.Command(t.binary, "show", "-json"), "show json state")
	if err != nil {
		return nil, err
	}

	json, err := capture.ToJson(false)
	if err != nil {
		return nil, err
	}
	return files.NewJsonFile(json), nil
}

func run(cmd *exec.Cmd, command string) (*capture, error) {
	capture := Capture(cmd)
	if err := cmd.Run(); err != nil {
		return capture, Error{
			Command: command,
			Go:      err,
			Binary:  capture.ToError(),
		}
	}
	return capture, nil
}
