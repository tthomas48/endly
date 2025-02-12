package exec

import (
	"fmt"
	"github.com/viant/endly"
	"github.com/viant/endly/model"
	"github.com/viant/endly/model/criteria"
	"github.com/viant/endly/util"
	"github.com/viant/toolbox"
	"github.com/viant/toolbox/cred"
	"github.com/viant/toolbox/data"
	"github.com/viant/toolbox/secret"
	"github.com/viant/toolbox/ssh"
	"github.com/viant/toolbox/url"
	"os"
	"path"
	"strings"
)

//ServiceID represent system executor service id
const ServiceID = "exec"

//SudoCredentialKey represent obsucated password sudo credentials key (target.Credentials)
const SudoCredentialKey = "**sudo**"

type execService struct {
	*endly.AbstractService
	credentials map[string]*cred.Config
}

func (s *execService) open(context *endly.Context, request *OpenSessionRequest) (*OpenSessionResponse, error) {
	var clientSession, err = s.openSession(context, request)
	if err != nil {
		return nil, err
	}
	return &OpenSessionResponse{
		SessionID: clientSession.ID,
	}, nil
}

func (s *execService) openSSHService(context *endly.Context, request *OpenSessionRequest) (ssh.Service, error) {
	if request.ReplayService != nil {
		return request.ReplayService, nil
	}
	target, err := context.ExpandResource(request.Target)
	if err != nil {
		return nil, err
	}
	authConfig, err := context.Secrets.GetOrCreate(target.Credentials)
	if err != nil {
		return nil, err
	}
	hostname, port := s.GetHostAndSSHPort(target)
	return ssh.NewService(hostname, port, authConfig)
}

func (s *execService) isSupportedScheme(target *url.Resource) bool {
	return target.ParsedURL.Scheme == "ssh" || target.ParsedURL.Scheme == "scp" || target.ParsedURL.Scheme == "file"
}

func (s *execService) initSession(context *endly.Context, target *url.Resource, session *model.Session, env map[string]string) error {
	//_, _ = s.changeDirectory(context, session, nil, target.ParsedURL.Path)
	for k, v := range env {
		if err := s.setEnvVariable(context, session, k, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *execService) openSession(context *endly.Context, request *OpenSessionRequest) (*model.Session, error) {
	target, err := context.ExpandResource(request.Target)
	if err != nil {
		return nil, err
	}
	if !s.isSupportedScheme(target) {
		return nil, fmt.Errorf("failed to open sessionID: invalid schema: %v in url: %v", target.ParsedURL.Scheme, target.URL)
	}

	s.Lock()
	sessions := TerminalSessions(context)
	s.Unlock()

	var replayCommands *ssh.ReplayCommands
	if request.Basedir != "" {
		replayCommands, err = ssh.NewReplayCommands(request.Basedir)
		if err != nil {
			return nil, err
		}
	}

	var sessionID = SessionID(context, target)
	if sessions.Has(sessionID) {
		s.Lock()
		SShSession := sessions[sessionID]
		s.Unlock()
		err = s.initSession(context, target, SShSession, request.Env)
		if err != nil {
			return nil, err
		}
		return SShSession, err
	}
	sshService, err := s.openSSHService(context, request)
	if err == nil {
		err = s.captureCommandIfNeeded(context, replayCommands, sshService)
	}
	if err != nil {
		return nil, err
	}
	SSHSession, err := model.NewSession(sessionID, sshService)
	if err != nil {
		return nil, err
	}
	if !request.Transient {
		context.Deffer(func() {
			_ = sshService.Close()
		})
	}
	SSHSession.MultiCommandSession, err = SSHSession.Service.OpenMultiCommandSession(request.Config)
	if err != nil {
		return nil, err
	}
	if !request.Transient {
		context.Deffer(func() {
			_, _ = s.closeSession(context, &CloseSessionRequest{
				SessionID: sessionID,
			})
		})
	}
	err = s.initSession(context, target, SSHSession, request.Env)
	if err != nil {
		return nil, err
	}
	s.Lock()
	sessions[sessionID] = SSHSession
	s.Unlock()
	SSHSession.Os, err = s.detectOperatingSystem(SSHSession)
	if err != nil {
		return nil, err
	}

	if currentDirectory, err := os.Getwd(); err == nil {
		if _, err = SSHSession.Run(fmt.Sprintf("cd %v", currentDirectory), nil, 0); err == nil {
			SSHSession.CurrentDirectory = currentDirectory
		}
	}
	return SSHSession, nil
}

func (s *execService) setEnvVariables(context *endly.Context, session *model.Session, env map[string]string) error {
	for k, v := range env {
		err := s.setEnvVariable(context, session, k, v)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *execService) setEnvVariable(context *endly.Context, session *model.Session, name, newValue string) error {
	newValue = context.Expand(newValue)

	if actual, has := session.EnvVariables[name]; has {
		if newValue == actual {
			return nil
		}
	}
	session.EnvVariables[name] = newValue
	var err error
	newValue = strings.TrimSpace(newValue)
	if strings.Contains(newValue, " ") {
		_, err = s.rumCommandTemplate(context, session, "export %v='%v'", name, newValue)
	} else {
		_, err = s.rumCommandTemplate(context, session, "export %v=%v", name, newValue)
	}
	return err
}

func (s *execService) changeDirectory(context *endly.Context, session *model.Session, commandInfo *RunResponse, directory string) (string, error) {
	if directory == "" {
		return "", nil
	}
	parent, name := path.Split(directory)
	if path.Ext(name) != "" {
		directory = parent
	}
	if len(directory) > 1 && strings.HasSuffix(directory, "/") {
		directory = string(directory[:len(directory)-1])
	}
	if session.CurrentDirectory == directory {
		return "", nil
	}

	result, err := s.rumCommandTemplate(context, session, "cd %v", directory)
	if err != nil {
		return "", err
	}

	if !util.CheckNoSuchFileOrDirectory(result) {
		session.CurrentDirectory = directory
	}
	return result, err
}

func (s *execService) run(context *endly.Context, session *model.Session, command string, listener ssh.Listener, timeoutMs int, terminators ...string) (stdout string, err error) {
	if stdout, err = session.Run(command, listener, timeoutMs, terminators...); err == nil {
		return stdout, err
	}
	if err == ssh.ErrTerminated {
		err := session.Reconnect()
		if err != nil {
			return "", err
		}
		currentDirectory := session.CurrentDirectory
		env := session.EnvVariables
		session.EnvVariables = make(map[string]string)
		session.CurrentDirectory = ""
		for k, v := range env {
			_ = s.setEnvVariable(context, session, k, v)
		}
		runResponse := &RunResponse{}
		_, _ = s.changeDirectory(context, session, runResponse, currentDirectory)
		return session.Run(command, listener, timeoutMs, terminators...)
	}
	return stdout, err
}

func (s *execService) rumCommandTemplate(context *endly.Context, session *model.Session, commandTemplate string, arguments ...interface{}) (string, error) {
	command := fmt.Sprintf(commandTemplate, arguments...)
	startEvent := s.Begin(context, NewSdtinEvent(session.ID, command))
	stdout, err := session.Run(command, nil, 1000)
	s.End(context)(startEvent, NewStdoutEvent(session.ID, stdout, err))
	return stdout, err
}

func (s *execService) applyCommandOptions(context *endly.Context, options *Options, session *model.Session, info *RunResponse) error {
	if len(options.SystemPaths) > 0 {
		session.Path.Unshift(options.SystemPaths...)
		if err := s.setEnvVariable(context, session, "PATH", session.Path.EnvValue()); err != nil {
			return err
		}
	}
	err := s.setEnvVariables(context, session, options.Env)
	if err != nil {
		return err
	}
	if options.Directory != "" {
		directory := context.Expand(options.Directory)
		_, err := s.changeDirectory(context, session, info, directory)
		if err != nil {
			return err
		}
	}
	return nil
}

func match(stdout string, candidates ...string) string {
	if len(candidates) == 0 {
		return ""
	}
	for _, candidate := range candidates {
		if util.EscapedContains(stdout, candidate) {
			return candidate
		}
	}
	return ""
}

func (s *execService) commandAsSuperUser(session *model.Session, command string) string {
	if session.Username == "root" {
		return command
	}
	if len(command) > 1 && !strings.Contains(command, "sudo") {
		return "sudo " + command
	}
	return command
}

func (s *execService) validateStdout(stdout string, command string, execution *ExtractCommand) error {
	errorMatch := match(stdout, execution.Errors...)
	if errorMatch != "" {
		return fmt.Errorf("encounter error fragment: (%v), command:%v, stdout: %v", errorMatch, command, stdout)
	}
	if len(execution.Success) > 0 {
		hasMatch := match(stdout, execution.Success...)
		if hasMatch == "" {
			return fmt.Errorf("failed to match any fragment: '%v', command: %v; stdout: %v", strings.Join(execution.Success, ","), command, stdout)
		}
	}
	return nil
}

func (s *execService) authSuperUserIfNeeded(stdout string, context *endly.Context, session *model.Session, extractCommand *ExtractCommand, response *RunResponse, request *ExtractRequest) (err error) {
	if session.SuperUSerAuth && !(util.EscapedContains(stdout, "Sorry, try again.") && util.EscapedContains(stdout, "Password")) {
		return nil
	}
	if util.EscapedContains(stdout, "Password") {
		session.SuperUSerAuth = true
		if len(request.Secrets) == 0 {
			request.Secrets = secret.NewSecrets(nil)
			request.Secrets[SudoCredentialKey] = secret.Secret(request.Target.Credentials)
		}
		extractCommand := NewExtractCommand(SudoCredentialKey, "", nil, []string{"Password", util.CommandNotFound})
		err = s.executeCommand(context, session, extractCommand, response, request)
	}
	return err
}

func (s *execService) buildExecutionState(response *RunResponse, context *endly.Context) data.Map {
	var state = context.State()
	var result = state.Clone()
	var commands = data.NewCollection()
	for _, log := range response.Cmd {
		var cmd = data.NewMap()
		cmd.Put("stdin", log.Stdin)
		cmd.Put("stdout", log.Stdout)
		commands.Push(cmd)
	}
	result.Put("cmd", commands)
	result.Put("output", response.Output)

	var stdout = ""
	if len(response.Cmd) > 0 {
		stdout = response.Cmd[len(response.Cmd)-1].Stdout
	}
	result.Put("stdout", stdout)
	return result
}

func hasTerminator(stdout string, terminators []string) bool {
	if len(terminators) == 0 {
		return false
	}
	for i := range terminators {
		if strings.Index(stdout, terminators[i]) != -1 {
			return true
		}
	}
	return false
}

func (s *execService) executeCommand(context *endly.Context, session *model.Session, extractCommand *ExtractCommand, response *RunResponse, request *ExtractRequest) (err error) {
	var state = context.State()
	state.SetValue("os.user", session.Username)

	securedCommand := context.Expand(extractCommand.Command)
	options := request.Options
	terminators := getTerminators(options, session, extractCommand)
	isSuperUserCmd := strings.Contains(securedCommand, "sudo ") || request.SuperUser

	if extractCommand.When != "" {
		var state = s.buildExecutionState(response, context)
		if ok, err := criteria.Evaluate(context, state, extractCommand.When, "Cmd.When", true); !ok {
			response.Add(NewCommandLog(securedCommand, "", err))
			return err
		}
	} else if strings.Contains(securedCommand, "$") {
		var state = s.buildExecutionState(response, context)
		securedCommand = state.ExpandAsText(securedCommand)
	}

	if isSuperUserCmd {
		if !session.SuperUSerAuth {
			terminators = append(terminators, "Password")
		}
		securedCommand = s.commandAsSuperUser(session, securedCommand)
	}

	var insecureCommand = securedCommand
	insecureCommand, err = context.Secrets.Expand(insecureCommand, request.Secrets)
	if err != nil {
		return err
	}

	var listener ssh.Listener

	//troubleshooting secrets - DO NOT USE unless really needed
	if os.Getenv("ENDLY_SECRET_REVEAL") == "true" {
		securedCommand = insecureCommand
	}
	s.Begin(context, NewSdtinEvent(session.ID, securedCommand))

	commandRetry := false
	listener = func(stdout string, hasMore bool) {
		if !commandRetry && request.AutoSudo && !util.IsPermitted(stdout) {
			return
		}
		if stdout != "" {
			context.Publish(NewStdoutEvent(session.ID, stdout, err))
		}
	}

	timeoutMs := options.TimeoutMs
	if extractCommand.TimeoutMs > 0 {
		timeoutMs = extractCommand.TimeoutMs
	}
	stdout, err := s.run(context, session, insecureCommand, listener, timeoutMs, terminators...)
	if len(response.Output) > 0 {
		if !strings.HasSuffix(response.Output, "\n") {
			response.Output += "\n"
		}
	}

	if request.AutoSudo && !util.IsPermitted(stdout) {
		commandRetry = true
		if session.Username != "root" && !strings.HasPrefix(securedCommand, "sudo") {
			stdout, err = s.retryWithSudo(context, session, insecureCommand, listener, options.TimeoutMs, terminators...)
			isSuperUserCmd = true
		}
	}

	if isSuperUserCmd {
		err = s.authSuperUserIfNeeded(stdout, context, session, extractCommand, response, request)
		if err != nil {
			return err
		}
	}
	response.Output += stdout

	if request.CheckError && !hasTerminator(stdout, terminators) {
		if errorCode, err := s.run(context, session, "echo $?", nil, options.TimeoutMs, terminators...); err == nil {
			exitStatus := toolbox.AsInt(strings.TrimSpace(errorCode))
			if exitStatus != 0 {
				return fmt.Errorf("exit code: %v, command: %v", exitStatus, securedCommand)
			}
		}
	}

	response.Add(NewCommandLog(securedCommand, stdout, err))
	if err != nil {
		return err
	}
	if err = s.validateStdout(stdout, securedCommand, extractCommand); err != nil {
		return err
	}

	stdout = response.Cmd[len(response.Cmd)-1].Stdout
	return extractCommand.Extract.Extract(context, response.Data, strings.Split(stdout, "\n")...)
}

func (s *execService) retryWithSudo(context *endly.Context, session *model.Session, command string, listener ssh.Listener, timeoutMs int, terminators ...string) (string, error) {
	terminators = append(terminators, "Password")
	command = s.commandAsSuperUser(session, command)
	return s.run(context, session, command, listener, timeoutMs, terminators...)
}

func getTerminators(options *Options, session *model.Session, execution *ExtractCommand) []string {
	var terminators = make([]string, 0)
	if len(execution.Terminators) > 0 {
		terminators = append(terminators, execution.Terminators...)
	} else {
		terminators = append(terminators, options.Terminators...)
	}
	terminators = append(terminators, "$ ")
	superUserPrompt := string(strings.Replace(session.ShellPrompt(), "$", "#", 1))
	if strings.Contains(superUserPrompt, "bash") {
		superUserPrompt = string(superUserPrompt[2:])
	}
	terminators = append(terminators, superUserPrompt)
	terminators = append(terminators, execution.Errors...)
	return terminators
}

func (s *execService) runCommands(context *endly.Context, request *RunRequest) (*RunResponse, error) {
	response, err := s.runExtractCommands(context, request.AsExtractRequest())
	if err != nil {
		return nil, err
	}
	if len(request.Extract) > 0 {
		if len(response.Data) == 0 {
			response.Data = data.NewMap()
		}
		err = request.Extract.Extract(context, response.Data, strings.Split(response.Output, "\n")...)
	}
	return response, err
}

func (s *execService) runExtractCommands(context *endly.Context, request *ExtractRequest) (*RunResponse, error) {
	target, err := context.ExpandResource(request.Target)
	if err != nil {
		return nil, err
	}
	session, err := s.openSession(context, &OpenSessionRequest{Target: target})
	if err != nil {
		return nil, err
	}

	response := NewRunResponse(session.ID)
	if err = s.applyCommandOptions(context, request.Options, session, response); err != nil {
		return nil, err
	}

	response = NewRunResponse(session.ID)
	for _, extractCommand := range request.Commands {
		var command = context.Expand(extractCommand.Command)
		if strings.Contains(command, "rm ") && strings.Contains(command, session.CurrentDirectory) {
			session.CurrentDirectory = "" //reset path
		}
		if strings.HasPrefix(command, "cd ") {
			if !strings.Contains(command, "&&") {
				var directory = strings.TrimSpace(string(command[3:]))
				stdout, err := s.changeDirectory(context, session, response, directory)
				response.Add(NewCommandLog(command, stdout, err))
				if err == nil {
					err = s.validateStdout(stdout, command, extractCommand)
				}
				if err != nil {
					return nil, err
				}
				continue
			}
			session.CurrentDirectory = "" //reset path
		}
		if strings.HasPrefix(command, "export ") {
			if !strings.Contains(command, "&&") {
				envVariable := string(command[7:])
				keyValuePair := strings.Split(envVariable, "=")
				if len(keyValuePair) == 2 {
					key := strings.TrimSpace(keyValuePair[0])
					value := strings.TrimSpace(keyValuePair[1])
					value = strings.Trim(value, "'\"")
					err = s.setEnvVariable(context, session, key, value)
					response.Add(NewCommandLog(command, "", err))
					continue
				}
			}
			session.EnvVariables = make(map[string]string) //reset env variables
		}
		err = s.executeCommand(context, session, extractCommand, response, request)
		if err != nil {
			return nil, err
		}

	}
	return response, nil
}

func (s *execService) closeSession(context *endly.Context, request *CloseSessionRequest) (*CloseSessionResponse, error) {
	clientSessions := TerminalSessions(context)
	if session, has := clientSessions[request.SessionID]; has {
		session.MultiCommandSession.Close()
		session.Close()
		delete(clientSessions, request.SessionID)
	}
	return &CloseSessionResponse{
		SessionID: request.SessionID,
	}, nil
}

func isAmd64Architecture(candidate string) bool {
	return strings.Contains(candidate, "amd64") || strings.Contains(candidate, "x86_64")
}

func (s *execService) extractOsPath(session *model.Session, os *model.OperatingSystem) error {
	output, err := session.Run("echo $PATH", nil, 0)
	if err != nil {
		return err
	}
	lines := strings.Split(output, "\r\n")
	for i := 0; i < len(lines); i++ {
		var line = lines[i]
		if !strings.Contains(line, ":") || !strings.Contains(line, "/") {
			continue
		}
		session.Path = model.NewPath(strings.Split(line, ":")...)
		break

	}
	return nil
}

func (s *execService) extractOsUser(session *model.Session, os *model.OperatingSystem) error {
	output, err := session.Run("echo $USER", nil, 0)
	if err != nil {
		return err
	}
	output = util.EscapeStdout(output)
	strings.Replace(output, "\n", "", len(output))
	session.Username = output
	return nil
}

func (s *execService) detectOperatingSystem(session *model.Session) (*model.OperatingSystem, error) {
	operatingSystem := &model.OperatingSystem{}
	session.Path = model.NewPath()

	varsionCheckCommand := "lsb_release -a"
	if session.MultiCommandSession.System() == "darwin" {
		varsionCheckCommand = "sw_vers"
	}
	output, err := session.Run(varsionCheckCommand, nil, 0)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(output, "\r\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if isAmd64Architecture(line) {
			operatingSystem.Architecture = "amd64"
		}
		pair := strings.Split(line, ":")
		if len(pair) != 2 {
			continue
		}

		var key = strings.Replace(strings.ToLower(pair[0]), " ", "", len(pair[0]))
		var val = strings.Replace(strings.Trim(pair[1], " \t\r"), " ", "", len(line))
		switch key {
		case "productname", "distributorid":
			operatingSystem.Name = strings.ToLower(val)
		case "productversion", "release":
			operatingSystem.Version = strings.ToLower(val)
		}

	}
	operatingSystem.Hardware, err = session.Run("uname -m", nil, 0)
	if err != nil {
		return nil, err
	}
	operatingSystem.Arch = operatingSystem.Architecture
	if isAmd64Architecture(operatingSystem.Hardware) {
		operatingSystem.Architecture = "amd64"
		operatingSystem.Arch = "x64"
	}
	if isArm64Architecture(operatingSystem.Hardware) {
		operatingSystem.Architecture = "arm64"
		operatingSystem.Arch = "aarch64"
	}

	operatingSystem.System = session.System()
	if err = s.extractOsPath(session, operatingSystem); err == nil {
		err = s.extractOsUser(session, operatingSystem)
	}
	return operatingSystem, err
}

func isArm64Architecture(hardware string) bool {
	return strings.Contains(hardware, "aarch64")
}

func (s *execService) captureCommandIfNeeded(context *endly.Context, replayCommands *ssh.ReplayCommands, sshService ssh.Service) (err error) {
	if replayCommands != nil {
		err = replayCommands.Enable(sshService)
		if err != nil {
			return err
		}
		context.Deffer(func() {
			_ = replayCommands.Store()
		})
	}
	return err
}

func (service *execService) setTarget(context *endly.Context, request *SetTargetRequest) (*SetTargetResponse, error) {
	target, err := context.ExpandResource(request.Resource)
	if err != nil {
		return nil, err
	}
	SetDefaultTarget(context, target)
	return &SetTargetResponse{}, nil
}

const (
	execServiceOpenExample = `{
  "Target": {
    "URL": "scp://127.0.0.1/",
    "Credentials": "${env.HOME}/.secret/localhost.json"
  },
  
  "SystemPaths": ["/usr/local/bin"],
  "Env": {
    "GOPATH":"${env.HOME}/go"
  }
}`
	execServiceRunExample = `{
  "Target": {
    "URL": "scp://127.0.0.1/",
    "Credentials": "${env.HOME}/.secret/localhost.json"
  },
  "Commands":["mkdir /tmp/app1"]
}`

	execServiceRunAndExtractExample = `{
	"Target": {
	"URL": "scp://127.0.0.1/",
	"Credentials": "${env.HOME}/.secret/localhost.json"
	},
	"SystemPaths": [
	"/opt/sdk/go/bin"
	],
	"Commands": [
	  {
		"Command": "go version",
		"Extract": [
		  {
			"RegExpr": "go(\\d\\.\\d)",
			"Key": "Version"
		  }
		]
	  }
	]
}`

	execServiceManagedCloseExample = `{
  "Target": {
    "URL": "scp://127.0.0.1/",
    "Credentials": "${env.HOME}/.secret/localhost.json"
  }
}`
)

func (s *execService) registerRoutes() {
	s.Register(&endly.Route{
		Action: "open",
		RequestInfo: &endly.ActionInfo{
			Description: "open SSH session, usually no need for using this action directly since run,extract actions open session if needed",
			Examples: []*endly.UseCase{
				{
					Description: "open session",
					Data:        execServiceOpenExample,
				},
			},
		},
		RequestProvider: func() interface{} {
			return &OpenSessionRequest{}
		},
		ResponseProvider: func() interface{} {
			return &OpenSessionResponse{}
		},
		Handler: func(context *endly.Context, request interface{}) (interface{}, error) {
			if req, ok := request.(*OpenSessionRequest); ok {
				return s.open(context, req)
			}
			return nil, fmt.Errorf("unsupported request type: %T", request)
		},
	})

	s.Register(&endly.Route{
		Action: "run",
		RequestInfo: &endly.ActionInfo{
			Description: "run terminal command",

			Examples: []*endly.UseCase{
				{
					Description: "run command",
					Data:        execServiceRunExample,
				},
			},
		},
		RequestProvider: func() interface{} {
			return &RunRequest{}
		},
		ResponseProvider: func() interface{} {
			return &RunResponse{}
		},
		Handler: func(context *endly.Context, request interface{}) (interface{}, error) {
			if req, ok := request.(*RunRequest); ok {
				return s.runCommands(context, req)
			}
			return nil, fmt.Errorf("unsupported request type: %T", request)
		},
	})

	s.Register(&endly.Route{
		Action: "extract",
		RequestInfo: &endly.ActionInfo{
			Description: "run terminal command and extract data from the stdout",

			Examples: []*endly.UseCase{
				{
					Description: "run and extract command",
					Data:        execServiceRunAndExtractExample,
				},
			},
		},
		RequestProvider: func() interface{} {
			return &ExtractRequest{}
		},
		ResponseProvider: func() interface{} {
			return &RunResponse{}
		},
		Handler: func(context *endly.Context, request interface{}) (interface{}, error) {
			if req, ok := request.(*ExtractRequest); ok {
				return s.runExtractCommands(context, req)
			}
			return nil, fmt.Errorf("unsupported request type: %T", request)
		},
	})

	s.Register(&endly.Route{
		Action: "close",
		RequestInfo: &endly.ActionInfo{
			Description: "close SSH terminal session, if created by run or extract it is scheduled to be closed at the end of endly run context.Close()",

			Examples: []*endly.UseCase{
				{
					Description: "close ",
					Data:        execServiceManagedCloseExample,
				},
			},
		},
		RequestProvider: func() interface{} {
			return &CloseSessionRequest{}
		},
		ResponseProvider: func() interface{} {
			return &CloseSessionResponse{}
		},
		Handler: func(context *endly.Context, request interface{}) (interface{}, error) {
			if req, ok := request.(*CloseSessionRequest); ok {
				return s.closeSession(context, req)
			}
			return nil, fmt.Errorf("unsupported request type: %T", request)
		},
	})

	s.Register(&endly.Route{
		Action: "setTarget",
		RequestInfo: &endly.ActionInfo{
			Description: "set default target",
			Examples:    []*endly.UseCase{},
		},
		RequestProvider: func() interface{} {
			return &SetTargetRequest{}
		},
		ResponseProvider: func() interface{} {
			return &SetTargetResponse{}
		},
		Handler: func(context *endly.Context, request interface{}) (interface{}, error) {
			if req, ok := request.(*SetTargetRequest); ok {
				return s.setTarget(context, req)
			}
			return nil, fmt.Errorf("unsupported request type: %T", request)
		},
	})
}

//New creates a new execution service
func New() endly.Service {
	var result = &execService{
		credentials:     make(map[string]*cred.Config),
		AbstractService: endly.NewAbstractService(ServiceID),
	}
	result.AbstractService.Service = result
	result.registerRoutes()
	return result
}
