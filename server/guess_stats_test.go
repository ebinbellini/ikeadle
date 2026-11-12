package server

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain keeps every test away from the real ikeadle.db in the repository.
func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "ikeadle-guess-test-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("GUESS_DB_PATH", filepath.Join(directory, "guesses.db")); err != nil {
		panic(err)
	}

	code := m.Run()

	guessStoreMu.Lock()
	if sharedGuessStore != nil {
		sharedGuessStore.Close()
		sharedGuessStore = nil
	}
	guessStoreMu.Unlock()

	os.RemoveAll(directory)
	os.Exit(code)
}

func openTestGuessStore(t *testing.T) *guessStore {
	t.Helper()

	store, err := openGuessStore(filepath.Join(t.TempDir(), "guesses.db"))
	if err != nil {
		t.Fatalf("openGuessStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close guess store: %v", err)
		}
	})
	return store
}

// useGuessStoreForTest points the package-level store at a test database and
// restores the previous store when the test finishes.
func useGuessStoreForTest(t *testing.T, store *guessStore) {
	t.Helper()

	guessStoreMu.Lock()
	previousStore := sharedGuessStore
	previousErr := guessStoreErr
	sharedGuessStore = store
	guessStoreErr = nil
	guessStoreMu.Unlock()

	t.Cleanup(func() {
		guessStoreMu.Lock()
		sharedGuessStore = previousStore
		guessStoreErr = previousErr
		guessStoreMu.Unlock()
	})
}

func refreshAndLoadGuessStats(t *testing.T, store *guessStore) guessStatsView {
	t.Helper()

	if err := store.refresh(context.Background()); err != nil {
		t.Fatalf("refresh guess stats: %v", err)
	}
	view, err := store.load(context.Background())
	if err != nil {
		t.Fatalf("load guess stats: %v", err)
	}
	return view
}

func TestGuessStoreAggregates(t *testing.T) {
	store := openTestGuessStore(t)

	productA := Product{
		ID:           "a",
		Name:         "Förvaringsvara",
		Price:        100,
		CategoryPath: "Förvaring / Hyllor",
	}
	productB := Product{
		ID:           "b",
		Name:         "Soffa",
		Price:        200,
		CategoryPath: "Soffor & fåtöljer / Soffor",
	}

	for _, guess := range []struct {
		mode    string
		product Product
		value   float64
	}{
		{guessModeDaily, productA, 80},  // -20 %
		{guessModeSlump, productA, 120}, // +20 %
		{guessModeStrid, productA, 100}, // 0 %
		{guessModeDaily, productB, 100}, // -50 %
	} {
		if err := store.record(guess.mode, guess.product, guess.value); err != nil {
			t.Fatalf("record %s guess: %v", guess.mode, err)
		}
	}

	ctx := context.Background()
	if err := store.refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	view, err := store.load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !view.HasGuesses {
		t.Fatal("view has no guesses")
	}
	if got, want := view.TotalGuesses, 4; got != want {
		t.Errorf("TotalGuesses = %d, want %d", got, want)
	}
	if got, want := view.UnderCount, 2; got != want {
		t.Errorf("UnderCount = %d, want %d", got, want)
	}
	if got, want := view.OverCount, 1; got != want {
		t.Errorf("OverCount = %d, want %d", got, want)
	}
	if got, want := view.ExactCount, 1; got != want {
		t.Errorf("ExactCount = %d, want %d", got, want)
	}
	if math.Abs(view.MedianError-(-0.1)) > 1e-9 {
		t.Errorf("MedianError = %v, want -0.1", view.MedianError)
	}
	if got, want := view.MedianErrorText, "−10"+nbsp+"%"; got != want {
		t.Errorf("MedianErrorText = %q, want %q", got, want)
	}
	if got, want := view.MedianErrorDirection, "under"; got != want {
		t.Errorf("MedianErrorDirection = %q, want %q", got, want)
	}
	if view.Updated == "" {
		t.Error("Updated is empty")
	}

	if len(view.Categories) != 2 {
		t.Fatalf("Categories length = %d, want 2", len(view.Categories))
	}

	first := view.Categories[0]
	if got, want := first.Name, "Förvaring"; got != want {
		t.Errorf("first category = %q, want %q", got, want)
	}
	if got, want := first.Guesses, 3; got != want {
		t.Errorf("first category guesses = %d, want %d", got, want)
	}
	if got, want := first.UnderShareText, "33"+nbsp+"%"; got != want {
		t.Errorf("first category under share = %q, want %q", got, want)
	}
	if got, want := first.OverShareText, "33"+nbsp+"%"; got != want {
		t.Errorf("first category over share = %q, want %q", got, want)
	}
	if got, want := first.MedianErrorDirection, "exact"; got != want {
		t.Errorf("first category direction = %q, want %q", got, want)
	}

	second := view.Categories[1]
	if got, want := second.Name, "Soffor & fåtöljer"; got != want {
		t.Errorf("second category = %q, want %q", got, want)
	}
	if got, want := second.MedianErrorText, "−50"+nbsp+"%"; got != want {
		t.Errorf("second category median = %q, want %q", got, want)
	}
	if got, want := second.MedianErrorDirection, "under"; got != want {
		t.Errorf("second category direction = %q, want %q", got, want)
	}
	if got, want := second.LeftPercent, 0.0; got != want {
		t.Errorf("second category left = %v, want %v", got, want)
	}
	if got, want := second.WidthPercent, 50.0; got != want {
		t.Errorf("second category width = %v, want %v", got, want)
	}
	if !second.LowSample {
		t.Error("single-guess category was not marked as a low sample")
	}

	if len(view.ScaleTicks) != 5 {
		t.Fatalf("ScaleTicks length = %d, want 5", len(view.ScaleTicks))
	}
	if got, want := view.ScaleTicks[0].Label, "−50"+nbsp+"%"; got != want {
		t.Errorf("first scale tick = %q, want %q", got, want)
	}
	if got, want := view.ScaleTicks[2].Label, "0"; got != want {
		t.Errorf("middle scale tick = %q, want %q", got, want)
	}
	if got, want := view.ScaleTicks[4].Position, 100.0; got != want {
		t.Errorf("last scale tick position = %v, want %v", got, want)
	}
}

func TestGuessStoreEmptyAggregate(t *testing.T) {
	store := openTestGuessStore(t)
	ctx := context.Background()

	if err := store.refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	view, err := store.load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if view.HasGuesses {
		t.Error("empty store reports guesses")
	}
	if view.TotalGuesses != 0 {
		t.Errorf("TotalGuesses = %d, want 0", view.TotalGuesses)
	}
	if view.Updated == "" {
		t.Error("empty store has no update timestamp")
	}
}

func TestSlumpGuessIsRecorded(t *testing.T) {
	store := openTestGuessStore(t)
	useGuessStoreForTest(t, store)

	product := Product{
		ID:           "slump-guess",
		Name:         "Testvara",
		Price:        100,
		ImageURL:     "https://example.com/test.png",
		CategoryPath: "Förvaring / Hyllor",
	}
	setProductPoolForTest(t, []Product{product})

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/slump/guess",
		strings.NewReader(`{"id":"slump-guess","guess":80}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	SlumpGuessHandler(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("SlumpGuessHandler status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}

	ctx := context.Background()
	if err := store.refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	view, err := store.load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got, want := view.TotalGuesses, 1; got != want {
		t.Fatalf("TotalGuesses = %d, want %d", got, want)
	}
	if got, want := view.MedianErrorText, "−20"+nbsp+"%"; got != want {
		t.Errorf("MedianErrorText = %q, want %q", got, want)
	}
	if len(view.Categories) != 1 || view.Categories[0].Name != "Förvaring" {
		t.Fatalf("categories = %+v, want one Förvaring row", view.Categories)
	}
}

func TestStatsPageShowsGuessStatistics(t *testing.T) {
	store := openTestGuessStore(t)
	useGuessStoreForTest(t, store)

	product := Product{
		ID:           "page-guess",
		Name:         "Testvara",
		Price:        100,
		ImageURL:     "https://example.com/test.png",
		CategoryPath: "Förvaring / Hyllor",
	}
	setProductPoolForTest(t, []Product{product})

	if err := store.record(guessModeDaily, product, 80); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := store.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	response := httptest.NewRecorder()
	StatsPage(response, httptest.NewRequest(http.MethodGet, "/statistik", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("StatsPage status = %d, want %d", response.Code, http.StatusOK)
	}
	body := response.Body.String()
	for _, want := range []string{
		"Hur gissningarna landar",
		"Underskattar",
		"Överskattar",
		"−20",
		"1 gissning",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered guess statistics do not contain %q", want)
		}
	}
	if strings.Contains(body, "ZgotmplZ") {
		t.Error("rendered guess statistics contain an escaped style placeholder")
	}
}
