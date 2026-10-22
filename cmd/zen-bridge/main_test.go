package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountsListDoesNotExposeKeys(t *testing.T) {
	d := t.TempDir()
	accounts := filepath.Join(d, "accounts.json")
	key := "secret-synthetic-credential-1"
	if err := os.WriteFile(accounts, []byte(`{"models":["big-pickle"],"defaults":{"profile":"raw"},"api_keys":["`+key+`"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(d, "config.json")
	data, _ := json.Marshal(map[string]any{"accounts_file": accounts})
	if err := os.WriteFile(cfg, data, 0600); err != nil {
		t.Fatal(err)
	}
	oldArgs, oldOut := os.Args, os.Stdout
	defer func() { os.Args = oldArgs; os.Stdout = oldOut }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	os.Args = []string{"zen-bridge", "accounts", "list", "--config", cfg}
	os.Stdout = w
	runErr := run()
	w.Close()
	os.Stdout = oldOut
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if strings.Contains(string(out), key) || strings.Contains(string(out), "api_key") {
		t.Fatal("credential exposed")
	}
	var rows []map[string]any
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["enabled"] != true || rows[0]["index"] != float64(1) || rows[0]["model_count"] != float64(1) {
		t.Fatalf("unexpected account metadata: %s", out)
	}
	if len(rows[0]) != 4 || !strings.HasPrefix(rows[0]["id"].(string), "key_") {
		t.Fatal("unexpected output fields")
	}
}

func TestAccountsListSingleConfig(t *testing.T) {
	d := t.TempDir()
	cfg := filepath.Join(d, "config.json")
	key := "synthetic-inline-credential-1"
	data := `{"models":["big-pickle"],"defaults":{"profile":"raw"},"api_keys":["` + key + `"]}`
	if err := os.WriteFile(cfg, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	oldArgs, oldOut := os.Args, os.Stdout
	defer func() { os.Args = oldArgs; os.Stdout = oldOut }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	os.Args = []string{"zen-bridge", "accounts", "list", "--config", cfg}
	os.Stdout = w
	runErr := run()
	w.Close()
	os.Stdout = oldOut
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if strings.Contains(string(b), key) {
		t.Fatal("secret exposed")
	}
	var rows []map[string]any
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["enabled"] != true || rows[0]["model_count"] != float64(1) {
		t.Fatalf("incorrect metadata: %s", b)
	}
}
