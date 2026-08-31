package server

import (
	"reflect"
	"testing"
)

func TestMergeExecArgvFallback(t *testing.T) {
	tpl := Template{DefaultArgv: []string{"python3", "app.py"}}

	argv, _, _, src, err := mergeExec(tpl, execReq{})
	if err != nil {
		t.Fatal(err)
	}
	if src != "template_default" || !reflect.DeepEqual(argv, []string{"python3", "app.py"}) {
		t.Fatalf("argv = %v src = %q", argv, src)
	}

	argv, _, _, src, err = mergeExec(tpl, execReq{Argv: []string{"ls"}})
	if err != nil || src != "request" || !reflect.DeepEqual(argv, []string{"ls"}) {
		t.Fatalf("request argv should win: %v %q %v", argv, src, err)
	}
}

func TestMergeExecNoArgvNoDefault(t *testing.T) {
	if _, _, _, _, err := mergeExec(Template{}, execReq{}); err != errNoArgv {
		t.Fatalf("err = %v, want errNoArgv", err)
	}
}

func TestMergeExecEnvAndDir(t *testing.T) {
	tpl := Template{
		Env: map[string]string{"PATH": "/img/bin", "LANG": "C"},
		Dir: "/srv",
	}
	req := execReq{
		Argv: []string{"true"},
		Env:  map[string]string{"PATH": "/req/bin"},
	}
	_, env, dir, _, err := mergeExec(tpl, req)
	if err != nil {
		t.Fatal(err)
	}
	if env["PATH"] != "/req/bin" {
		t.Errorf("request env must override: PATH = %q", env["PATH"])
	}
	if env["LANG"] != "C" {
		t.Errorf("template env must survive: LANG = %q", env["LANG"])
	}
	if dir != "/srv" {
		t.Errorf("dir = %q, want /srv from the template", dir)
	}

	_, _, dir, _, _ = mergeExec(tpl, execReq{Argv: []string{"true"}, Dir: "/"})
	if dir != "/" {
		t.Errorf("request dir must win: %q", dir)
	}
}

func TestMergeExecDoesNotMutateInputs(t *testing.T) {
	tplEnv := map[string]string{"A": "1"}
	tpl := Template{Env: tplEnv}
	req := execReq{Argv: []string{"true"}, Env: map[string]string{"B": "2"}}
	_, env, _, _, _ := mergeExec(tpl, req)
	env["C"] = "3"
	if _, ok := tplEnv["C"]; ok {
		t.Error("mergeExec mutated the template env map")
	}
	if _, ok := req.Env["C"]; ok {
		t.Error("mergeExec mutated the request env map")
	}
}
