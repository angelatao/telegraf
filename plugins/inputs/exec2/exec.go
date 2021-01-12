package exec2

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/plugins/inputs"
	"github.com/influxdata/telegraf/plugins/outputs"
	"github.com/influxdata/telegraf/plugins/parsers"
	"github.com/influxdata/telegraf/plugins/parsers/nagios"
	"github.com/kballard/go-shellquote"
)

const sampleConfig = `
  ## Commands array
  commands = [
    "/tmp/test.sh",
    "/usr/bin/mycollector --foo=bar",
    "/tmp/collect_*.sh"
  ]

  ## pattern as argument for netstat find pid (ie, "netstat -anvp tcp|grep LISTEN|grep '\\<%s\\>' |awk '{print $9}'")
  pattern = "netstat -anvp tcp|grep LISTEN|grep '\\<%s\\>' |awk '{print $9}'"
  ## The listening port number of the process
  listen_ports ="80,8082"
  
  ## Timeout for each command to complete.
  timeout = "5s"

  ## measurement name suffix (for separating different commands)
  name_suffix = "_mycollector"

  ## Data format to consume.
  ## Each data format has its own unique set of configuration options, read
  ## more about them here:
  ## https://github.com/influxdata/telegraf/blob/master/docs/DATA_FORMATS_INPUT.md
  data_format = "influx"
`

const MaxStderrBytes = 512

type Exec2 struct {
	Commands []string
	Command  string

	Pattern      string
	Ports        string            `toml:"listen_ports"`
	cmds         map[string]string //<cmd, port>
	addedPattern bool

	ExCommands []string
	ex_cmds    map[string]string
	mutext     sync.RWMutex

	Timeout internal.Duration

	parser parsers.Parser

	runner Runner
	Log    telegraf.Logger `toml:"-"`
}

func NewExec2() *Exec2 {
	return &Exec2{
		runner:  CommandRunner{},
		Timeout: internal.Duration{Duration: time.Second * 5},
	}
}

type Runner interface {
	Run(string, time.Duration) ([]byte, []byte, error)
}

type CommandRunner struct{}

func (c CommandRunner) Run(
	command string,
	timeout time.Duration,
) ([]byte, []byte, error) {
	split_cmd, err := shellquote.Split(command)
	if err != nil || len(split_cmd) == 0 {
		return nil, nil, fmt.Errorf("exec2: unable to parse command, %s", err)
	}

	cmd := exec.Command(split_cmd[0], split_cmd[1:]...)

	var (
		out    bytes.Buffer
		stderr bytes.Buffer
	)
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	runErr := internal.RunTimeout(cmd, timeout)

	out = removeCarriageReturns(out)
	if stderr.Len() > 0 {
		stderr = removeCarriageReturns(stderr)
		stderr = truncate(stderr)
	}

	return out.Bytes(), stderr.Bytes(), runErr
}

func truncate(buf bytes.Buffer) bytes.Buffer {
	// Limit the number of bytes.
	didTruncate := false
	if buf.Len() > MaxStderrBytes {
		buf.Truncate(MaxStderrBytes)
		didTruncate = true
	}
	if i := bytes.IndexByte(buf.Bytes(), '\n'); i > 0 {
		// Only show truncation if the newline wasn't the last character.
		if i < buf.Len()-1 {
			didTruncate = true
		}
		buf.Truncate(i)
	}
	if didTruncate {
		buf.WriteString("...")
	}
	return buf
}

// removeCarriageReturns removes all carriage returns from the input if the
// OS is Windows. It does not return any errors.
func removeCarriageReturns(b bytes.Buffer) bytes.Buffer {
	if runtime.GOOS == "windows" {
		var buf bytes.Buffer
		for {
			byt, er := b.ReadBytes(0x0D)
			end := len(byt)
			if nil == er {
				end -= 1
			}
			if nil != byt {
				buf.Write(byt[:end])
			} else {
				break
			}
			if nil != er {
				break
			}
		}
		b = buf
	}
	return b

}

func (e *Exec2) ProcessCommand(command string, acc telegraf.Accumulator, wg *sync.WaitGroup) {
	defer wg.Done()
	_, isNagios := e.parser.(*nagios.NagiosParser)

	out, errbuf, runErr := e.runner.Run(command, e.Timeout.Duration)
	if !isNagios && runErr != nil {
		err := fmt.Errorf("exec2: %s for command '%s': %s", runErr, command, string(errbuf))
		acc.AddError(err)
		return
	}

	metrics, err := e.parser.Parse(out)
	if err != nil {
		acc.AddError(err)
		return
	}

	if isNagios {
		metrics, err = nagios.TryAddState(runErr, metrics)
		if err != nil {
			e.Log.Errorf("Failed to add nagios state: %s", err)
		}
	}

	for _, m := range metrics {
		e.addMetric(command, m, acc)
	}
}

func (e *Exec2) addMetric(command string, metric telegraf.Metric, acc telegraf.Accumulator) {
	// add port tag support
	if port, ok := e.cmds[command]; ok {
		metric.AddTag("port", port)
	}

	e.addExMetric(command, metric)

	acc.AddMetric(metric)
}

func (e *Exec2) addExMetric(command string, metric telegraf.Metric) {
	e.mutext.RLock()
	defer e.mutext.RUnlock()

	// add ex port tag support
	if port, ok := e.ex_cmds[command]; ok {
		metric.AddTag("port", port)
	}
}

func (e *Exec2) SampleConfig() string {
	return sampleConfig
}

func (e *Exec2) Description() string {
	return "Read metrics from one or more commands that can output to stdout"
}

func (e *Exec2) SetParser(parser parsers.Parser) {
	e.parser = parser
}

func (e *Exec2) Gather(acc telegraf.Accumulator) error {
	var wg sync.WaitGroup

	// Legacy single command support
	if e.Command != "" {
		e.Commands = append(e.Commands, e.Command)
		e.Command = ""
	}

	commands := make([]string, 0, len(e.Commands))
	for _, pattern := range e.Commands {
		cmdAndArgs := strings.SplitN(pattern, " ", 2)
		if len(cmdAndArgs) == 0 {
			continue
		}

		matches, err := filepath.Glob(cmdAndArgs[0])
		if err != nil {
			acc.AddError(err)
			continue
		}

		if len(matches) == 0 {
			// There were no matches with the glob pattern, so let's assume
			// that the command is in PATH and just run it as it is
			commands = append(commands, pattern)
		} else {
			// There were matches, so we'll append each match together with
			// the arguments to the commands slice
			for _, match := range matches {
				if len(cmdAndArgs) == 1 {
					commands = append(commands, match)
				} else {
					commands = append(commands,
						strings.Join([]string{match, cmdAndArgs[1]}, " "))
				}
			}
		}
	}

	exCommands := e.readExCommandsLock(acc)

	wg.Add(len(commands) + len(exCommands))
	for _, command := range commands {
		go e.ProcessCommand(command, acc, &wg)
	}
	for _, command := range exCommands {
		go e.ProcessCommand(command, acc, &wg)
	}
	wg.Wait()
	return nil
}

// readCommandsLock mutext read commands
func (e *Exec2) readExCommandsLock(acc telegraf.Accumulator) []string {
	e.mutext.RLock()
	defer e.mutext.RUnlock()

	commands := make([]string, 0, len(e.ExCommands))
	for _, pattern := range e.ExCommands {
		cmdAndArgs := strings.SplitN(pattern, " ", 2)
		if len(cmdAndArgs) == 0 {
			continue
		}

		matches, err := filepath.Glob(cmdAndArgs[0])
		if err != nil {
			acc.AddError(err)
			continue
		}

		if len(matches) == 0 {
			// There were no matches with the glob pattern, so let's assume
			// that the command is in PATH and just run it as it is
			commands = append(commands, pattern)
		} else {
			// There were matches, so we'll append each match together with
			// the arguments to the commands slice
			for _, match := range matches {
				if len(cmdAndArgs) == 1 {
					commands = append(commands, match)
				} else {
					commands = append(commands,
						strings.Join([]string{match, cmdAndArgs[1]}, " "))
				}
			}
		}
	}
	return commands
}

// addPatternCommandsLock parse ports generate multi command by the specified pattern
func (e *Exec2) addPatternCommands() {
	if e.Pattern != "" && e.Ports != "" && !e.addedPattern {
		ports := strings.Split(e.Ports, ",")
		commands := make([]string, 0, len(ports))
		e.cmds = make(map[string]string, len(ports))
		for _, port := range ports {
			cmd := fmt.Sprintf(e.Pattern, port)
			e.cmds[cmd] = port
			commands = append(commands, cmd)
			e.addedPattern = true
		}
		e.Commands = append(e.Commands, commands...)
	}
}

// Connect satisfies the Ouput interface.
func (e *Exec2) Connect() error {
	return nil
}

// Close satisfies the Ouput interface.
func (e *Exec2) Close() error {
	return nil
}

// Write writes the metrics to the configured command.
// receive http_listener_v2 metrics add commands.
func (e *Exec2) Write(metrics []telegraf.Metric) error {
	fmt.Println("Received msg...")
	exPorts := make([]string, 0)

	for i, m := range metrics {
		fmt.Printf("Received metrics[%d]: %v \n", i, m)
		fields := m.FieldList()
		for _, f := range fields {
			if value, ok := f.Value.(float64); ok {
				exPorts = append(exPorts, strconv.FormatFloat(value, 'f', -1, 64))
			}
		}
		fmt.Printf("Received fields:[%v] \n", exPorts)
	}

	fmt.Printf("Exec2 commands: %v \n", e.Commands)

	if e.Pattern != "" && len(exPorts) > 0 {
		// write lock
		e.mutext.Lock()
		defer e.mutext.Unlock()

		// clear e.ExCommands
		e.ExCommands = make([]string, 0)

		commands := make([]string, 0, len(exPorts))
		e.ex_cmds = make(map[string]string, len(exPorts))
		for _, port := range exPorts {
			cmd := fmt.Sprintf(e.Pattern, port)
			e.ex_cmds[cmd] = port
			commands = append(commands, cmd)
		}

		e.ExCommands = append(e.ExCommands, commands...)
	}

	return nil
}

func (e *Exec2) Init() error {
	// Legacy pattern command support
	e.addPatternCommands()
	return nil
}

func init() {
	exec := NewExec2()
	inputs.Add("exec2", func() telegraf.Input {
		return exec
	})

	outputs.Add("exec2", func() telegraf.Output {
		return exec
	})
}
