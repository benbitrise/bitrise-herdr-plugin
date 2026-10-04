package sshconf

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func paths(t *testing.T) Paths {
	t.Helper()
	return DefaultPaths(t.TempDir())
}

func TestWriteScanRemove(t *testing.T) {
	p := paths(t)
	e := Entry{Alias: "rde-a", SessionID: "s1", HostName: "h.example", Port: 2200, User: "vagrant", IdentityFile: "/k/id"}
	changed, err := p.Write(e)
	if err != nil || !changed {
		t.Fatalf("write: %v %v", changed, err)
	}
	st, _ := os.Stat(filepath.Join(p.Dir, "rde-a"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", st.Mode().Perm())
	}
	dst, _ := os.Stat(p.Dir)
	if dst.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", dst.Mode().Perm())
	}
	if changed, _ := p.Write(e); changed {
		t.Fatal("rewriting an identical entry should be a no-op")
	}

	// A hand-written file with the prefix is reported but never managed.
	os.WriteFile(filepath.Join(p.Dir, "rde-mine"), []byte("Host rde-mine\n  HostName x\n"), 0o644)
	managed, unmanaged, err := p.Scan("rde-")
	if err != nil {
		t.Fatal(err)
	}
	if got := managed["rde-a"]; got != e {
		t.Fatalf("scan: got %+v", got)
	}
	if !unmanaged["rde-mine"] || len(managed) != 1 {
		t.Fatalf("scan: managed=%v unmanaged=%v", managed, unmanaged)
	}
	if _, err := p.Write(Entry{Alias: "rde-mine", SessionID: "s2", HostName: "y", Port: 22, User: "u"}); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("overwriting a hand-written file: %v", err)
	}
	if err := p.Remove("rde-mine"); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("removing a hand-written file: %v", err)
	}
	if err := p.Remove("rde-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.Dir, "rde-a")); !os.IsNotExist(err) {
		t.Fatal("file still there")
	}
}

func TestEnsureIncludeCreatesConfig(t *testing.T) {
	p := paths(t)
	changed, err := p.EnsureInclude("rde-", false)
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	data, _ := os.ReadFile(p.ConfigFile)
	if !strings.Contains(string(data), "Include "+p.IncludePattern("rde-")+"\n") {
		t.Fatalf("got %q", data)
	}
	if changed, _ := p.EnsureInclude("rde-", false); changed {
		t.Fatal("second call should be a no-op")
	}
}

func TestEnsureIncludeKeepsExistingTildeLine(t *testing.T) {
	p := paths(t)
	os.MkdirAll(filepath.Dir(p.ConfigFile), 0o700)
	orig := "Include /other/ssh_config\nInclude ~/.ssh/config.d/rde-*\n\nHost vagrant*\n  HostName 127.0.0.1\n"
	os.WriteFile(p.ConfigFile, []byte(orig), 0o644)
	if changed, err := p.EnsureInclude("rde-", false); err != nil || changed {
		t.Fatalf("an Include above every Host block is fine as is: %v %v", changed, err)
	}
	data, _ := os.ReadFile(p.ConfigFile)
	if string(data) != orig {
		t.Fatal("file was modified")
	}
}

func TestEnsureIncludeMovesMisplacedLine(t *testing.T) {
	p := paths(t)
	os.MkdirAll(filepath.Dir(p.ConfigFile), 0o700)
	orig := "Host work\n  HostName w.example\n\nInclude ~/.ssh/config.d/rde-*\n"
	os.WriteFile(p.ConfigFile, []byte(orig), 0o640)

	if changed, _ := p.EnsureInclude("rde-", true); !changed {
		t.Fatal("dry run should report the needed change")
	}
	if data, _ := os.ReadFile(p.ConfigFile); string(data) != orig {
		t.Fatal("dry run modified the file")
	}

	if changed, err := p.EnsureInclude("rde-", false); err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	data, _ := os.ReadFile(p.ConfigFile)
	s := string(data)
	inc := strings.Index(s, "Include ")
	if inc < 0 || inc > strings.Index(s, "Host work") || strings.Count(s, "config.d/rde-*") != 1 {
		t.Fatalf("got %q", s)
	}
	if !strings.Contains(s, "Host work\n  HostName w.example\n") {
		t.Fatalf("existing Host block lost: %q", s)
	}
	st, _ := os.Stat(p.ConfigFile)
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("mode not preserved: %v", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(p.ConfigFile + ".bak-bitrise-herdr"); string(b) != orig {
		t.Fatal("no backup of the original")
	}
}

func TestEnsureIncludeWritesThroughSymlink(t *testing.T) {
	p := paths(t)
	real := filepath.Join(t.TempDir(), "dotfiles", "ssh_config")
	os.MkdirAll(filepath.Dir(real), 0o700)
	os.WriteFile(real, []byte("Host work\n  HostName w.example\n"), 0o600)
	os.MkdirAll(filepath.Dir(p.ConfigFile), 0o700)
	if err := os.Symlink(real, p.ConfigFile); err != nil {
		t.Fatal(err)
	}
	if changed, err := p.EnsureInclude("rde-", false); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if fi, err := os.Lstat(p.ConfigFile); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced: %v", err)
	}
	data, _ := os.ReadFile(real)
	if !strings.HasPrefix(strings.SplitN(string(data), "\n", 2)[1], "Include ") {
		t.Fatalf("target not updated:\n%s", data)
	}
}

func TestEnsureIncludeReadOnlySymlinkTarget(t *testing.T) {
	p := paths(t)
	dir := filepath.Join(t.TempDir(), "store")
	os.MkdirAll(dir, 0o700)
	real := filepath.Join(dir, "ssh_config")
	os.WriteFile(real, []byte("Host work\n"), 0o400)
	os.Chmod(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	os.MkdirAll(filepath.Dir(p.ConfigFile), 0o700)
	os.Symlink(real, p.ConfigFile)

	_, err := p.EnsureInclude("rde-", false)
	if err == nil || !strings.Contains(err.Error(), "Include ") {
		t.Fatalf("want an error with the line to add by hand, got %v", err)
	}
	if fi, _ := os.Lstat(p.ConfigFile); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced")
	}
}
