package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	content := "# comment\n\nuser.name = Ramon de Vera Jr.\nuser.email = ramondevera@gmail.com\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := loadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if m["user.name"] != "Ramon de Vera Jr." {
		t.Errorf("user.name = %q, want %q", m["user.name"], "Ramon de Vera Jr.")
	}
	if m["user.email"] != "ramondevera@gmail.com" {
		t.Errorf("user.email = %q, want %q", m["user.email"], "ramondevera@gmail.com")
	}
}

func TestLoadConfigFileMissing(t *testing.T) {
	m, err := loadConfigFile(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Fatalf("expected empty map, got %v", m)
	}
}

func TestLoadConfigFileMalformedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("no equals sign here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigFile(path); err == nil {
		t.Fatal("expected an error for a malformed line, got nil")
	}
}

func TestLoadConfigFileValueContainsEquals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("user.name = a = b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if m["user.name"] != "a = b" {
		t.Errorf("user.name = %q, want %q (split on first '=' only)", m["user.name"], "a = b")
	}
}

func TestSaveConfigFileRoundTripsAndSorts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config")
	m := map[string]string{"user.email": "ramondevera@gmail.com", "user.name": "Ramon"}
	if err := saveConfigFile(path, m); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "user.email = ramondevera@gmail.com\nuser.name = Ramon\n"
	if string(data) != want {
		t.Errorf("saved content = %q, want %q", data, want)
	}

	got, err := loadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got["user.name"] != "Ramon" || got["user.email"] != "ramondevera@gmail.com" {
		t.Errorf("round trip mismatch: %v", got)
	}
}

func TestResolveConfigValueRepoOverridesGlobal(t *testing.T) {
	repoPath := filepath.Join(t.TempDir(), "config")
	globalPath := filepath.Join(t.TempDir(), "config")

	if err := saveConfigFile(repoPath, map[string]string{"user.email": "repo@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := saveConfigFile(globalPath, map[string]string{
		"user.name":  "Global Name",
		"user.email": "global@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	// user.email: repo-local wins.
	v, err := resolveConfigValue(repoPath, globalPath, "user.email")
	if err != nil {
		t.Fatal(err)
	}
	if v != "repo@example.com" {
		t.Errorf("user.email = %q, want repo-local value %q", v, "repo@example.com")
	}

	// user.name: absent locally, falls through to global — per-key
	// cascading, not per-file.
	v, err = resolveConfigValue(repoPath, globalPath, "user.name")
	if err != nil {
		t.Fatal(err)
	}
	if v != "Global Name" {
		t.Errorf("user.name = %q, want global fallback %q", v, "Global Name")
	}
}

func TestResolveConfigValueUnsetEverywhere(t *testing.T) {
	v, err := resolveConfigValue(
		filepath.Join(t.TempDir(), "config"),
		filepath.Join(t.TempDir(), "config"),
		"user.name",
	)
	if err != nil {
		t.Fatal(err)
	}
	if v != "" {
		t.Errorf("expected empty string for an unset key, got %q", v)
	}
}

func TestFormatAuthor(t *testing.T) {
	if got := formatAuthor("Ramon", "ramondevera@gmail.com"); got != "Ramon <ramondevera@gmail.com>" {
		t.Errorf("formatAuthor(name, email) = %q, want %q", got, "Ramon <ramondevera@gmail.com>")
	}
	if got := formatAuthor("Ramon", ""); got != "Ramon" {
		t.Errorf("formatAuthor(name, \"\") = %q, want %q", got, "Ramon")
	}
	// Neither set: falls back to the OS username (non-empty on any
	// sane test host) rather than asserting an exact value.
	if got := formatAuthor("", ""); got == "" {
		t.Error("formatAuthor(\"\", \"\") returned empty string, want an OS-username or \"unknown\" fallback")
	}
}

func TestSetAndGetConfigValueRepoLocal(t *testing.T) {
	r := newTestRepo(t)

	if err := saveConfigFile(repoConfigPath(r), map[string]string{"user.name": "Ramon de Vera Jr."}); err != nil {
		t.Fatal(err)
	}

	got, err := resolvedAuthorField(r, "user.name")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Ramon de Vera Jr." {
		t.Errorf("resolvedAuthorField(user.name) = %q, want %q", got, "Ramon de Vera Jr.")
	}
}
