package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeFile кладёт файл во временный каталог и возвращает путь
func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func devicesFrom(t *testing.T, body string) ([]Device, error) {
	t.Helper()
	p := writeFile(t, t.TempDir(), "devices.db", body)
	return readDevices(p, 22)
}

func TestReadDevices_NoOptionKeepsGlobalCommands(t *testing.T) {
	devs, err := devicesFrom(t, "r1\n10.0.0.5:2222\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 || devs[0].Commands != nil || devs[1].Commands != nil {
		t.Fatalf("no override expected, got %+v", devs)
	}
	if devs[1].Name != "10.0.0.5:2222" || devs[1].Port != 2222 {
		t.Fatalf("port parsing broke: %+v", devs[1])
	}
}

func TestReadDevices_CommandsOption(t *testing.T) {
	dir := t.TempDir()
	cf := writeFile(t, dir, "junos.cmds", "# comment\nshow version\n\nshow chassis routing\n")
	db := writeFile(t, dir, "devices.db", "r1 commands="+cf+"\nr2\n")

	devs, err := readDevices(db, 22)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"show version", "show chassis routing"}
	if !reflect.DeepEqual(devs[0].Commands, want) {
		t.Fatalf("r1 commands = %v, want %v", devs[0].Commands, want)
	}
	if devs[0].Name != "r1" || devs[0].Host != "r1" || devs[0].Port != 22 {
		t.Fatalf("name/host/port must not include options: %+v", devs[0])
	}
	if devs[1].Commands != nil {
		t.Fatalf("r2 must keep global commands, got %v", devs[1].Commands)
	}
}

func TestReadDevices_OptionWithPort(t *testing.T) {
	dir := t.TempDir()
	cf := writeFile(t, dir, "c", "sh ver\n")
	db := writeFile(t, dir, "devices.db",
		"host:2222 commands="+cf+"\n[2001:db8::1]:4001\tcommands="+cf+"\n")

	devs, err := readDevices(db, 22)
	if err != nil {
		t.Fatal(err)
	}
	if devs[0].Name != "host:2222" || devs[0].Host != "host" || devs[0].Port != 2222 {
		t.Fatalf("bad v4/host+port: %+v", devs[0])
	}
	if devs[1].Name != "[2001:db8::1]:4001" || devs[1].Host != "2001:db8::1" || devs[1].Port != 4001 {
		t.Fatalf("bad ipv6+port (tab separator): %+v", devs[1])
	}
	if len(devs[0].Commands) != 1 || len(devs[1].Commands) != 1 {
		t.Fatalf("override missing: %+v", devs)
	}
}

func TestReadDevices_RelativePathIsFromCwd(t *testing.T) {
	cwd := t.TempDir()
	writeFile(t, cwd, "local.cmds", "sh ver\n")
	// devices.db лежит в ДРУГОМ каталоге — путь всё равно считается от cwd
	db := writeFile(t, t.TempDir(), "devices.db", "r1 commands=local.cmds\n")
	t.Chdir(cwd)

	devs, err := readDevices(db, 22)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(devs[0].Commands, []string{"sh ver"}) {
		t.Fatalf("got %v", devs[0].Commands)
	}
}

func TestReadDevices_Errors(t *testing.T) {
	dir := t.TempDir()
	empty := writeFile(t, dir, "empty", "# only a comment\n\n")
	cases := map[string]string{
		"missing file":   "r1 commands=" + filepath.Join(dir, "nope") + "\n",
		"empty file":     "r1 commands=" + empty + "\n",
		"unknown option": "r1 colour=red\n",
		"no value":       "r1 commands=\n",
		"not key=value":  "r1 commands\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := devicesFrom(t, body)
			if err == nil {
				t.Fatal("expected error before any connection is made")
			}
			if !strings.Contains(err.Error(), "r1") {
				t.Fatalf("error must name the offending device line: %v", err)
			}
		})
	}
}

func TestDeviceCmds_OverrideWinsOverGlobal(t *testing.T) {
	global := []string{"g1"}
	if got := (Device{}).cmds(global); !reflect.DeepEqual(got, global) {
		t.Fatalf("got %v", got)
	}
	d := Device{Commands: []string{"o1", "o2"}}
	if got := d.cmds(global); !reflect.DeepEqual(got, d.Commands) {
		t.Fatalf("got %v", got)
	}
}

func TestDropOverrides(t *testing.T) {
	devs := []Device{{Name: "a", Commands: []string{"x"}}, {Name: "b"}}
	if n := dropOverrides(devs); n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
	if devs[0].Commands != nil {
		t.Fatal("override must be cleared (-C wins)")
	}
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b)
}

func TestPrintPlan_SingleSetKeepsOldFormat(t *testing.T) {
	out := captureStdout(t, func() {
		printPlan([]string{"sh ver"}, []Device{{Name: "a"}, {Name: "b"}})
	})
	want := "\nCommands to run:\n  sh ver\n\nDevices (2):\na b\n\n"
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestPrintPlan_GroupsDevicesByEffectiveCommands(t *testing.T) {
	devs := []Device{
		{Name: "a"},
		{Name: "c", Commands: []string{"special"}},
		{Name: "b"},
	}
	out := captureStdout(t, func() { printPlan([]string{"sh ver"}, devs) })

	order := []string{"set 1/2", "sh ver", "a b", "set 2/2", "special", "Devices (1):\nc"}
	pos := -1
	for _, s := range order {
		i := strings.Index(out[pos+1:], s)
		if i < 0 {
			t.Fatalf("%q not found in order after pos %d:\n%s", s, pos, out)
		}
		pos += 1 + i
	}
}
