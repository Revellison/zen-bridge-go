package accounts

import (
	"testing"

	"zenbridge/internal/config"
)

func testAccount(id string, models ...string) config.Account {
	return config.Account{ID: id, Enabled: true, AuthMode: "key", APIKey: "key-" + id, Models: models}
}

func TestPoolModels(t *testing.T) {
	a := testAccount("one", "b", "a")
	disabled := testAccount("two", "c")
	disabled.Enabled = false
	p := NewPool([]config.Account{a, disabled})
	models := p.Models()
	if len(models) != 2 || models[0] != "a" || models[1] != "b" {
		t.Fatalf("models %v", models)
	}
	if !p.Ready() {
		t.Fatal("enabled account not ready")
	}
	noModels := testAccount("plain")
	if p := NewPool([]config.Account{noModels}); !p.Ready() {
		t.Fatal("account without model list not ready")
	}
	if p := NewPool([]config.Account{disabled}); p.Ready() {
		t.Fatal("disabled-only pool ready")
	}
}

func TestPoolAcquireAffinityAndFallback(t *testing.T) {
	a, b := testAccount("a", "m"), testAccount("b", "m")
	p := NewPool([]config.Account{a, b})
	first, release, err := p.Acquire("m", "session")
	if err != nil {
		t.Fatal(err)
	}
	release()
	second, release, err := p.Acquire("m", "session")
	if err != nil || second.ID != first.ID {
		t.Fatalf("affinity switched: %s %s %v", first.ID, second.ID, err)
	}
	release()
	if _, release, err := p.Acquire("not-listed", ""); err != nil {
		t.Fatalf("unknown model is not routable: %v", err)
	} else {
		release()
	}
	p.Reload([]config.Account{})
	if _, _, err := p.Acquire("m", ""); err != ErrNoAvailable {
		t.Fatal("empty pool accepted")
	}
}

func TestAcquireAccount(t *testing.T) {
	a := config.Account{ID: "one", Enabled: true, Models: []string{"manual"}}
	disabled := config.Account{ID: "two", Enabled: false}
	p := NewPool([]config.Account{a, disabled})
	_, release, err := p.AcquireAccount(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	if _, _, err := p.AcquireAccount(disabled.ID); err != ErrNoAvailable {
		t.Fatal("disabled account accepted")
	}
}
