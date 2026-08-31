package server

import "errors"

// errNoArgv reports that an exec request sent no argv and the template has no
// default.
var errNoArgv = errors.New("argv is required and the template has no default_argv")

// mergeExec folds a template's exec defaults under a request. A request value
// always wins. argvSource is "request" or "template_default".
func mergeExec(tpl Template, req execReq) (argv []string, env map[string]string, dir, argvSource string, err error) {
	argv = req.Argv
	argvSource = "request"
	if len(argv) == 0 {
		argv = tpl.DefaultArgv
		argvSource = "template_default"
	}
	if len(argv) == 0 {
		return nil, nil, "", "", errNoArgv
	}

	env = make(map[string]string, len(tpl.Env)+len(req.Env))
	for k, v := range tpl.Env {
		env[k] = v
	}
	for k, v := range req.Env {
		env[k] = v
	}

	dir = req.Dir
	if dir == "" {
		dir = tpl.Dir
	}
	return argv, env, dir, argvSource, nil
}
