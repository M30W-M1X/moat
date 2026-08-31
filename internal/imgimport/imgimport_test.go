package imgimport

import (
	"reflect"
	"testing"
)

func TestParseImageConfig(t *testing.T) {
	im := inspectImage{}
	im.Config.Env = []string{"PATH=/usr/local/bin:/usr/bin", "LANG=C.UTF-8", "EMPTY="}
	im.Config.WorkingDir = "/app"
	im.Config.Entrypoint = []string{"/entrypoint.sh"}
	im.Config.Cmd = []string{"python3", "app.py"}

	env, dir, argv, warns := parseImageConfig(im)

	wantEnv := map[string]string{"PATH": "/usr/local/bin:/usr/bin", "LANG": "C.UTF-8", "EMPTY": ""}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Errorf("env = %v, want %v", env, wantEnv)
	}
	if dir != "/app" {
		t.Errorf("dir = %q, want /app", dir)
	}
	if want := []string{"/entrypoint.sh", "python3", "app.py"}; !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %v, want %v", argv, want)
	}
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none", warns)
	}
}

func TestParseImageConfigNoDefaultCommand(t *testing.T) {
	_, dir, argv, warns := parseImageConfig(inspectImage{})
	if dir != "" || argv != nil {
		t.Errorf("dir = %q argv = %v, want empty", dir, argv)
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want one about no default command", warns)
	}
}
