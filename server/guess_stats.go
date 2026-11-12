package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Guess statistics are stored separately from the game state. The raw table
// keeps one anonymous row per valid guess; a daily job rolls it up into the
// category table that /statistik reads. Nothing in either table identifies a
// player: there is no name, account, IP address, lobby code or browser token.
const (
	guessModeDaily = "daily"
	guessModeSlump = "slump"
	guessModeStrid = "strid"

	defaultGuessDBPath       = "ikeadle.db"
	guessStatsUpdatedLayout  = "2006-01-02 15:04"
	guessLowSampleLimit      = 10
	guessChartMinimumPercent = 20.0
	guessChartMaximumPercent = 100.0
)

// guessStore owns the SQLite connection. One connection is enough for this
// workload, and serialising access avoids SQLITE_BUSY between readers and the
// daily aggregation job.
type guessStore struct {
	db        *sql.DB
	refreshMu sync.Mutex
}

var (
	guessStoreMu     sync.Mutex
	sharedGuessStore *guessStore
	guessStoreErr    error

	guessStoreWarnMu sync.Mutex
	guessStoreWarned bool
)

// guessStatsView is everything templates/pages/statistik.html needs from the
// aggregated guess table.
type guessStatsView struct {
	Error bool
	// ErrorText is shown when the database exists but cannot be read.
	ErrorText string

	HasGuesses bool

	TotalGuesses int
	UnderCount   int
	OverCount    int
	ExactCount   int

	TotalGuessesText     string
	UnderShareText       string
	OverShareText        string
	ExactShareText       string
	MedianError          float64
	MedianErrorText      string
	MedianErrorDirection string
	MeanAbsoluteText     string
	Updated              string

	Categories []guessCategoryStat
	ScaleTicks []guessScaleTick
	ScaleMax   float64
	Capped     bool
}

type guessCategoryStat struct {
	Name                 string
	Guesses              int
	Under                int
	Over                 int
	Exact                int
	MedianError          float64
	MeanAbsoluteError    float64
	GuessesText          string
	GuessLabel           string
	UnderShareText       string
	OverShareText        string
	MedianErrorText      string
	MedianErrorDirection string
	MeanAbsoluteText     string
	LowSample            bool

	LeftPercent  float64
	WidthPercent float64
	IsCapped     bool
}

type guessScaleTick struct {
	Label    string
	Position float64
}

func guessDBPath() string {
	path := strings.TrimSpace(os.Getenv("GUESS_DB_PATH"))
	if path == "" {
		return defaultGuessDBPath
	}
	return path
}

// getGuessStore opens the database lazily. A failed open is cached so a broken
// path does not turn every guess into a new error message.
func getGuessStore() (*guessStore, error) {
	guessStoreMu.Lock()
	defer guessStoreMu.Unlock()

	if sharedGuessStore != nil {
		return sharedGuessStore, nil
	}
	if guessStoreErr != nil {
		return nil, guessStoreErr
	}

	store, err := openGuessStore(guessDBPath())
	if err != nil {
		guessStoreErr = err
		return nil, err
	}
	sharedGuessStore = store
	return store, nil
}

func warnGuessStore(err error) {
	guessStoreWarnMu.Lock()
	defer guessStoreWarnMu.Unlock()

	if guessStoreWarned {
		return
	}
	guessStoreWarned = true
	fmt.Printf("Guess statistics are unavailable: %v\n", err)
}

func openGuessStore(path string) (*guessStore, error) {
	if strings.TrimSpace(path) == "" {
		path = defaultGuessDBPath
	}

	memory := path == ":memory:"
	if !memory {
		if directory := filepath.Dir(path); directory != "." && directory != "" {
			if err := os.MkdirAll(directory, 0o755); err != nil {
				return nil, fmt.Errorf("create guess database directory: %w", err)
			}
		}
	}

	dsn := path
	if !memory {
		// WAL lets the page read old aggregates while the daily job replaces
		// them. busy_timeout gives a short grace period if the two meet.
		dsn = "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open guess database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping guess database: %w", err)
	}

	store := &guessStore{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}

	if !memory {
		// The data is anonymous, but there is no reason to make it readable to
		// every user on a shared machine.
		if err := os.Chmod(path, 0o600); err != nil {
			fmt.Printf("Could not restrict guess database permissions: %v\n", err)
		}
	}

	return store, nil
}

func (s *guessStore) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS guesses (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at INTEGER NOT NULL,
			mode TEXT NOT NULL,
			product_id TEXT NOT NULL,
			category TEXT NOT NULL,
			product_price REAL NOT NULL,
			guess REAL NOT NULL,
			relative_error REAL NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS guesses_created_at_idx ON guesses (created_at)`,
		`CREATE INDEX IF NOT EXISTS guesses_category_idx ON guesses (category)`,
		`CREATE TABLE IF NOT EXISTS guess_category_stats (
			category TEXT PRIMARY KEY,
			guesses INTEGER NOT NULL,
			under_count INTEGER NOT NULL,
			over_count INTEGER NOT NULL,
			exact_count INTEGER NOT NULL,
			median_relative_error REAL NOT NULL,
			mean_absolute_error REAL NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS guess_stats_meta (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			total_guesses INTEGER NOT NULL,
			under_count INTEGER NOT NULL,
			over_count INTEGER NOT NULL,
			exact_count INTEGER NOT NULL,
			median_relative_error REAL NOT NULL,
			mean_absolute_error REAL NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
	}

	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate guess database: %w", err)
		}
	}
	return nil
}

func (s *guessStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// record stores one valid guess. relative_error is signed: negative means the
// player guessed below the price.
func (s *guessStore) record(mode string, product Product, guess float64) error {
	price := product.Price
	if price <= 0 || guess <= 0 {
		return nil
	}

	relativeError := (guess - price) / price
	_, err := s.db.Exec(
		`INSERT INTO guesses
			(created_at, mode, product_id, category, product_price, guess, relative_error)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		time.Now().Unix(),
		mode,
		product.ID,
		topLevelCategory(product.CategoryPath),
		price,
		guess,
		relativeError,
	)
	return err
}

// recordGuess never changes the response to a player. A database problem is
// logged once and the game continues without statistics.
func recordGuess(mode string, product Product, guess float64) {
	store, err := getGuessStore()
	if err != nil {
		warnGuessStore(err)
		return
	}
	if err := store.record(mode, product, guess); err != nil {
		fmt.Printf("Could not save guess statistics: %v\n", err)
	}
}

// RefreshGuessStats rebuilds the aggregate tables from every raw guess. It runs
// at startup and from the daily cron job.
func RefreshGuessStats() error {
	store, err := getGuessStore()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return store.refresh(ctx)
}

// loadGuessStats reads the materialised aggregates. The page never scans the
// raw guesses table, so it stays fast as the log grows.
func loadGuessStats() guessStatsView {
	store, err := getGuessStore()
	if err != nil {
		warnGuessStore(err)
		return guessStatsView{
			Error:     true,
			ErrorText: "Gissningsstatistiken kunde inte läsas just nu.",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	view, err := store.load(ctx)
	if err != nil {
		fmt.Printf("Could not read guess statistics: %v\n", err)
		return guessStatsView{
			Error:     true,
			ErrorText: "Gissningsstatistiken kunde inte läsas just nu.",
		}
	}
	return view
}

type guessCategoryAggregate struct {
	Category     string
	Guesses      int
	Under        int
	Over         int
	Exact        int
	Median       float64
	MeanAbsolute float64
}

func (s *guessStore) refresh(ctx context.Context) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin guess statistics update: %w", err)
	}
	defer tx.Rollback() // ignored after a successful commit

	var total, under, over, exact int
	var meanAbsolute float64
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN relative_error < 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN relative_error > 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN relative_error = 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(AVG(ABS(relative_error)), 0)
		FROM guesses`).Scan(&total, &under, &over, &exact, &meanAbsolute); err != nil {
		return fmt.Errorf("count guesses: %w", err)
	}

	var medianError float64
	if total > 0 {
		if err := tx.QueryRowContext(ctx, `
			WITH ordered AS (
				SELECT relative_error,
				       ROW_NUMBER() OVER (ORDER BY relative_error) AS rn,
				       COUNT(*) OVER () AS cnt
				FROM guesses
			)
			SELECT AVG(relative_error)
			FROM ordered
			WHERE rn IN ((cnt + 1) / 2, (cnt + 2) / 2)`).Scan(&medianError); err != nil {
			return fmt.Errorf("calculate median guess error: %w", err)
		}
	}

	categoryRows, err := tx.QueryContext(ctx, `
		SELECT category,
		       COUNT(*),
		       COALESCE(SUM(CASE WHEN relative_error < 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN relative_error > 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN relative_error = 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(AVG(ABS(relative_error)), 0)
		FROM guesses
		GROUP BY category`)
	if err != nil {
		return fmt.Errorf("count guesses per category: %w", err)
	}

	byCategory := make(map[string]*guessCategoryAggregate)
	for categoryRows.Next() {
		aggregate := &guessCategoryAggregate{}
		if err := categoryRows.Scan(
			&aggregate.Category,
			&aggregate.Guesses,
			&aggregate.Under,
			&aggregate.Over,
			&aggregate.Exact,
			&aggregate.MeanAbsolute,
		); err != nil {
			categoryRows.Close()
			return fmt.Errorf("scan category guess counts: %w", err)
		}
		byCategory[aggregate.Category] = aggregate
	}
	if err := categoryRows.Err(); err != nil {
		categoryRows.Close()
		return fmt.Errorf("read category guess counts: %w", err)
	}
	categoryRows.Close()

	if len(byCategory) > 0 {
		medianRows, err := tx.QueryContext(ctx, `
			WITH ordered AS (
				SELECT category,
				       relative_error,
				       ROW_NUMBER() OVER (PARTITION BY category ORDER BY relative_error) AS rn,
				       COUNT(*) OVER (PARTITION BY category) AS cnt
				FROM guesses
			)
			SELECT category, AVG(relative_error)
			FROM ordered
			WHERE rn IN ((cnt + 1) / 2, (cnt + 2) / 2)
			GROUP BY category`)
		if err != nil {
			return fmt.Errorf("calculate category median errors: %w", err)
		}

		for medianRows.Next() {
			var category string
			var median float64
			if err := medianRows.Scan(&category, &median); err != nil {
				medianRows.Close()
				return fmt.Errorf("scan category median error: %w", err)
			}
			if aggregate := byCategory[category]; aggregate != nil {
				aggregate.Median = median
			}
		}
		if err := medianRows.Err(); err != nil {
			medianRows.Close()
			return fmt.Errorf("read category median errors: %w", err)
		}
		medianRows.Close()
	}

	aggregates := make([]guessCategoryAggregate, 0, len(byCategory))
	for _, aggregate := range byCategory {
		aggregates = append(aggregates, *aggregate)
	}
	sort.Slice(aggregates, func(i, j int) bool {
		if aggregates[i].Guesses != aggregates[j].Guesses {
			return aggregates[i].Guesses > aggregates[j].Guesses
		}
		return aggregates[i].Category < aggregates[j].Category
	})

	if _, err := tx.ExecContext(ctx, `DELETE FROM guess_category_stats`); err != nil {
		return fmt.Errorf("clear category guess statistics: %w", err)
	}
	if len(aggregates) > 0 {
		statement, err := tx.PrepareContext(ctx, `
			INSERT INTO guess_category_stats
				(category, guesses, under_count, over_count, exact_count, median_relative_error, mean_absolute_error)
			VALUES (?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare category guess statistics: %w", err)
		}
		for _, aggregate := range aggregates {
			if _, err := statement.ExecContext(ctx,
				aggregate.Category,
				aggregate.Guesses,
				aggregate.Under,
				aggregate.Over,
				aggregate.Exact,
				aggregate.Median,
				aggregate.MeanAbsolute,
			); err != nil {
				statement.Close()
				return fmt.Errorf("insert category guess statistics: %w", err)
			}
		}
		statement.Close()
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM guess_stats_meta`); err != nil {
		return fmt.Errorf("clear guess statistics summary: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO guess_stats_meta
			(id, total_guesses, under_count, over_count, exact_count, median_relative_error, mean_absolute_error, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?)`,
		total, under, over, exact, medianError, meanAbsolute, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("write guess statistics summary: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit guess statistics update: %w", err)
	}
	return nil
}

func (s *guessStore) load(ctx context.Context) (guessStatsView, error) {
	// A read transaction makes the summary and the category rows come from the
	// same aggregate snapshot, even if the daily job commits in between.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return guessStatsView{}, fmt.Errorf("begin guess statistics read: %w", err)
	}
	defer tx.Rollback()

	var total, under, over, exact int
	var medianError, meanAbsolute float64
	var updatedAt int64

	err = tx.QueryRowContext(ctx, `
		SELECT total_guesses, under_count, over_count, exact_count,
		       median_relative_error, mean_absolute_error, updated_at
		FROM guess_stats_meta
		WHERE id = 1`).Scan(
		&total, &under, &over, &exact, &medianError, &meanAbsolute, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return guessStatsView{}, nil
	}
	if err != nil {
		return guessStatsView{}, fmt.Errorf("read guess statistics summary: %w", err)
	}

	view := guessStatsView{
		HasGuesses:           total > 0,
		TotalGuesses:         total,
		UnderCount:           under,
		OverCount:            over,
		ExactCount:           exact,
		TotalGuessesText:     formatNumber(total),
		UnderShareText:       formatShare(under, total),
		OverShareText:        formatShare(over, total),
		ExactShareText:       formatShare(exact, total),
		MedianError:          medianError,
		MedianErrorText:      formatSignedPercent(medianError),
		MedianErrorDirection: errorDirection(medianError),
		MeanAbsoluteText:     formatPercent(meanAbsolute),
		Updated:              time.Unix(updatedAt, 0).Format(guessStatsUpdatedLayout),
	}
	if total == 0 {
		return view, nil
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT category, guesses, under_count, over_count, exact_count,
		       median_relative_error, mean_absolute_error
		FROM guess_category_stats
		ORDER BY guesses DESC, category ASC`)
	if err != nil {
		return guessStatsView{}, fmt.Errorf("read category guess statistics: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		category := guessCategoryStat{}
		if err := rows.Scan(
			&category.Name,
			&category.Guesses,
			&category.Under,
			&category.Over,
			&category.Exact,
			&category.MedianError,
			&category.MeanAbsoluteError,
		); err != nil {
			return guessStatsView{}, fmt.Errorf("scan category guess statistics: %w", err)
		}

		category.GuessesText = formatNumber(category.Guesses)
		category.GuessLabel = formatGuessCount(category.Guesses)
		category.UnderShareText = formatShare(category.Under, category.Guesses)
		category.OverShareText = formatShare(category.Over, category.Guesses)
		category.MedianErrorText = formatSignedPercent(category.MedianError)
		category.MedianErrorDirection = errorDirection(category.MedianError)
		category.MeanAbsoluteText = formatPercent(category.MeanAbsoluteError)
		category.LowSample = category.Guesses < guessLowSampleLimit
		view.Categories = append(view.Categories, category)
	}
	if err := rows.Err(); err != nil {
		return guessStatsView{}, fmt.Errorf("read category guess statistics: %w", err)
	}

	view.ScaleMax, view.Capped = applyGuessChartScale(view.Categories)
	view.ScaleTicks = guessScaleTicks(view.ScaleMax)
	return view, nil
}

// applyGuessChartScale puts each category's median error on a symmetric,
// diverging chart. Categories with very few guesses are ignored when choosing
// the scale so one noisy row cannot flatten the rest.
func applyGuessChartScale(categories []guessCategoryStat) (float64, bool) {
	maxAbs := 0.0
	for _, category := range categories {
		if category.Guesses >= 5 && math.Abs(category.MedianError) > maxAbs {
			maxAbs = math.Abs(category.MedianError)
		}
	}
	if maxAbs == 0 {
		for _, category := range categories {
			if math.Abs(category.MedianError) > maxAbs {
				maxAbs = math.Abs(category.MedianError)
			}
		}
	}

	scale := math.Ceil(maxAbs*100/10) * 10
	if scale < guessChartMinimumPercent {
		scale = guessChartMinimumPercent
	}
	if scale > guessChartMaximumPercent {
		scale = guessChartMaximumPercent
	}

	capped := false
	for index := range categories {
		magnitude := math.Abs(categories[index].MedianError) * 100
		if magnitude > scale {
			magnitude = scale
			capped = true
		}

		width := magnitude / scale * 50
		categories[index].WidthPercent = roundedPercent(width)
		categories[index].LeftPercent = 50
		categories[index].IsCapped = math.Abs(categories[index].MedianError)*100 > scale
		if categories[index].MedianErrorDirection == "under" {
			categories[index].LeftPercent = roundedPercent(50 - width)
		}
	}

	return scale, capped
}

func guessScaleTicks(scale float64) []guessScaleTick {
	return []guessScaleTick{
		{Label: formatGuessScalePercent(-scale), Position: 0},
		{Label: formatGuessScalePercent(-scale / 2), Position: 25},
		{Label: "0", Position: 50},
		{Label: formatGuessScalePercent(scale / 2), Position: 75},
		{Label: formatGuessScalePercent(scale), Position: 100},
	}
}

// errorDirection classifies a signed relative error. Values that round to
// zero percent are shown as exact so the label and colour agree.
func errorDirection(relativeError float64) string {
	if math.Abs(relativeError) < 0.005 {
		return "exact"
	}
	if relativeError < 0 {
		return "under"
	}
	return "over"
}

// formatSignedPercent turns a fraction into, for example, "−18 %" or "+7 %".
func formatSignedPercent(relativeError float64) string {
	percent := relativeError * 100
	if math.Abs(percent) < 0.5 {
		return "0" + nbsp + "%"
	}

	sign := "+"
	if percent < 0 {
		sign = "−"
		percent = -percent
	}
	return sign + formatNumber(int(math.Round(percent))) + nbsp + "%"
}

func formatPercent(relativeError float64) string {
	return formatNumber(int(math.Round(math.Abs(relativeError)*100))) + nbsp + "%"
}

func formatGuessCount(count int) string {
	if count == 1 {
		return "1 gissning"
	}
	return formatNumber(count) + " gissningar"
}

func formatGuessScalePercent(percent float64) string {
	if math.Abs(percent) < 0.5 {
		return "0"
	}

	sign := "+"
	if percent < 0 {
		sign = "−"
		percent = -percent
	}
	return sign + formatNumber(int(math.Round(percent))) + nbsp + "%"
}
