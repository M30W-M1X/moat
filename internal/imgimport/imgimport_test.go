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

func TestUUIDAndSeedFromNameAreStableAndDistinct(t *testing.T) {
	a1, a2 := uuidFromName("python"), uuidFromName("python")
	if a1 != a2 {
		t.Fatalf("uuidFromName not stable: %q vs %q", a1, a2)
	}
	if uuidFromName("python") == uuidFromName("node") {
		t.Fatal("uuidFromName collided for different names")
	}
	if uuidFromName("python") == seedFromName("python") {
		t.Fatal("uuid and seed must differ")
	}
	if m := len(a1); m != 36 {
		t.Fatalf("uuid length = %d, want 36 (%q)", m, a1)
	}
}

func TestAutoSizeBytes(t *testing.T) {
	cases := []struct{ in, want int64 }{
		{0, 64 << 20},
		{1 << 20, 64 << 20},            // tiny -> floor
		{200 << 20, 300 << 20},         // 200 MiB -> 300 MiB
		{201*1024*1024 + 1, 302 << 20}, // rounds up to a whole MiB
	}
	for _, c := range cases {
		if got := autoSizeBytes(c.in); got != c.want {
			t.Errorf("autoSizeBytes(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
