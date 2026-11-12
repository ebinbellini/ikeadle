package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testStatsProducts() []Product {
	scraped := time.Date(2026, time.November, 10, 6, 0, 0, 0, time.UTC)

	return []Product{
		{ID: "1", Name: "Billig förvaring", Price: 49.99, ImageURL: "https://example.com/1.png", CategoryPath: "Förvaring / Hyllor & bokhyllor / Bokhyllor", ScrapedAt: scraped},
		{ID: "2", Name: "Förvaring till 50", Price: 50, ImageURL: "https://example.com/2.png", CategoryPath: "Förvaring / Hyllor & bokhyllor / Bokhyllor", ScrapedAt: scraped},
		{ID: "3", Name: "Köksvara 199", Price: 199, ImageURL: "https://example.com/3.png", CategoryPath: "Kök & vitvaror / Köksutrustning", ScrapedAt: scraped},
		{ID: "4", Name: "Köksvara 200", Price: 200, ImageURL: "https://example.com/4.png", CategoryPath: "Kök & vitvaror / Köksutrustning", ScrapedAt: scraped},
		{ID: "5", Name: "Dyr soffa", Price: 10000, ImageURL: "https://example.com/5.png", CategoryPath: "Soffor & fåtöljer / Soffor", ScrapedAt: scraped},
		{ID: "6", Name: "Utan kategori", Price: 1000, ImageURL: "https://example.com/6.png", CategoryPath: "", ScrapedAt: scraped},
	}
}

func withStatsTestProducts(t *testing.T) {
	t.Helper()
	setProductPoolForTest(t, testStatsProducts())
}

func setProductPoolForTest(t *testing.T, list []Product) {
	t.Helper()

	productsMu.RLock()
	oldProducts := products
	oldIndex := productIndex
	productsMu.RUnlock()

	SetProducts(list)
	t.Cleanup(func() {
		productsMu.Lock()
		products = oldProducts
		productIndex = oldIndex
		productsMu.Unlock()
	})
}

func TestBuildStats(t *testing.T) {
	data := buildStats(testStatsProducts())

	if data.Empty {
		t.Fatal("buildStats marked a non-empty catalogue as empty")
	}
	if got, want := data.Summary.ProductCount, "6"; got != want {
		t.Errorf("ProductCount = %q, want %q", got, want)
	}
	if got, want := data.Summary.CategoryCount, "4"; got != want {
		t.Errorf("CategoryCount = %q, want %q", got, want)
	}
	if got, want := data.Summary.MedianPrice, "200"+nbsp+"kr"; got != want {
		t.Errorf("MedianPrice = %q, want %q", got, want)
	}
	if got, want := data.Summary.PriceRange, "50–10"+nbsp+"000"+nbsp+"kr"; got != want {
		t.Errorf("PriceRange = %q, want %q", got, want)
	}
	if got, want := data.Updated, "2026-11-10"; got != want {
		t.Errorf("Updated = %q, want %q", got, want)
	}
	if got, want := data.MedianBucket, "200–299"; got != want {
		t.Errorf("MedianBucket = %q, want %q", got, want)
	}

	wantCounts := map[string]int{
		"0–49":                              1,
		"50–99":                             1,
		"100–199":                           1,
		"200–299":                           1,
		"300–499":                           0,
		"500–749":                           0,
		"750–999":                           0,
		"1" + nbsp + "000–1" + nbsp + "499": 1,
		"1" + nbsp + "500–1" + nbsp + "999": 0,
		"2" + nbsp + "000–2" + nbsp + "999": 0,
		"3" + nbsp + "000–4" + nbsp + "999": 0,
		"5" + nbsp + "000–9" + nbsp + "999": 0,
		"10" + nbsp + "000+":                1,
	}

	bucketTotal := 0
	for _, bucket := range data.Buckets {
		bucketTotal += bucket.Count
		if got, want := bucket.Count, wantCounts[bucket.Label]; got != want {
			t.Errorf("bucket %q count = %d, want %d", bucket.Label, got, want)
		}
		if bucket.Count > 0 && bucket.BarPercent <= 0 {
			t.Errorf("bucket %q has products but BarPercent = %v", bucket.Label, bucket.BarPercent)
		}
		if bucket.IsMedian && bucket.Label != data.MedianBucket {
			t.Errorf("bucket %q marked as median, want %q", bucket.Label, data.MedianBucket)
		}
	}
	if bucketTotal != 6 {
		t.Errorf("bucket counts total = %d, want 6", bucketTotal)
	}

	if len(data.Categories) != 4 {
		t.Fatalf("Categories length = %d, want 4", len(data.Categories))
	}
	wantOrder := []string{"Soffor & fåtöljer", "Övrigt", "Kök & vitvaror", "Förvaring"}
	for index, want := range wantOrder {
		if got := data.Categories[index].Name; got != want {
			t.Errorf("Categories[%d].Name = %q, want %q", index, got, want)
		}
	}

	for _, category := range data.Categories {
		if category.BarPercent < 0 || category.BarPercent > 100 {
			t.Errorf("category %q BarPercent = %v, want 0-100", category.Name, category.BarPercent)
		}
	}
	if got, want := data.Categories[0].BarPercent, 100.0; got != want {
		t.Errorf("most expensive category BarPercent = %v, want %v", got, want)
	}
	if got, want := data.Categories[0].MedianText, "10"+nbsp+"000"+nbsp+"kr"; got != want {
		t.Errorf("most expensive category MedianText = %q, want %q", got, want)
	}

	if len(data.ScaleTicks) != 4 {
		t.Fatalf("ScaleTicks length = %d, want 4", len(data.ScaleTicks))
	}
	for index, want := range []string{"10", "100", "1" + nbsp + "000", "10" + nbsp + "000"} {
		if got := data.ScaleTicks[index].Label; got != want+nbsp+"kr" {
			t.Errorf("ScaleTicks[%d] = %q, want %q", index, got, want+nbsp+"kr")
		}
	}
	wantPositions := []float64{0, 33.3, 66.7, 100}
	for index, want := range wantPositions {
		if got := data.ScaleTicks[index].Position; got != want {
			t.Errorf("ScaleTicks[%d].Position = %v, want %v", index, got, want)
		}
	}
}

func TestBuildStatsEmpty(t *testing.T) {
	data := buildStats(nil)

	if !data.Empty {
		t.Fatal("buildStats did not mark an empty catalogue as empty")
	}
	if len(data.Buckets) != 0 || len(data.Categories) != 0 {
		t.Errorf("empty stats rendered buckets or categories: %d, %d", len(data.Buckets), len(data.Categories))
	}
}

func TestBuildStatsSkipsProductsWithoutPrice(t *testing.T) {
	data := buildStats([]Product{
		{ID: "1", Name: "Giltig", Price: 100, CategoryPath: "Förvaring"},
		{ID: "2", Name: "Utan pris", Price: 0, CategoryPath: "Förvaring"},
	})

	if got, want := data.Summary.ProductCount, "1"; got != want {
		t.Errorf("ProductCount = %q, want %q", got, want)
	}
	if got, want := data.Summary.CategoryCount, "1"; got != want {
		t.Errorf("CategoryCount = %q, want %q", got, want)
	}
}

func TestTopLevelCategory(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"Förvaring / Hyllor & bokhyllor / Bokhyllor", "Förvaring"},
		{"Badrum", "Badrum"},
		{"", "Övrigt"},
		{" / ", "Övrigt"},
		{"  Kök & vitvaror  /  Kökssystem", "Kök & vitvaror"},
	}

	for _, test := range tests {
		if got := topLevelCategory(test.path); got != test.want {
			t.Errorf("topLevelCategory(%q) = %q, want %q", test.path, got, test.want)
		}
	}
}

func TestFormatNumber(t *testing.T) {
	tests := map[int]string{
		0:      "0",
		12:     "12",
		999:    "999",
		1000:   "1" + nbsp + "000",
		10218:  "10" + nbsp + "218",
		100000: "100" + nbsp + "000",
	}

	for value, want := range tests {
		if got := formatNumber(value); got != want {
			t.Errorf("formatNumber(%d) = %q, want %q", value, got, want)
		}
	}
}

func TestStatsPageRendersCatalogueNumbers(t *testing.T) {
	withStatsTestProducts(t)

	request := httptest.NewRequest(http.MethodGet, "/statistik", nil)
	response := httptest.NewRecorder()
	StatsPage(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("StatsPage status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", got)
	}

	body := response.Body.String()
	for _, want := range []string{
		"<h1>Statistik</h1>",
		"Hur gissningarna landar",
		"Antal varor per prisnivå",
		"Prisnivåer per kategori",
		`aria-current="page"`,
		"Medianpriset",
		"Inga gissningar ännu",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page does not contain %q", want)
		}
	}

	if strings.Contains(body, "ZgotmplZ") {
		t.Error("rendered page contains an escaped style placeholder")
	}
}

func TestStatsRouteIsRegistered(t *testing.T) {
	withStatsTestProducts(t)

	mux := newMux()
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/statistik", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("GET /statistik status = %d, want %d", response.Code, http.StatusOK)
	}
	if !strings.Contains(response.Body.String(), "Medianpriset") {
		t.Error("GET /statistik did not render the statistics page")
	}
}

func TestStatsPageRendersEmptyState(t *testing.T) {
	setProductPoolForTest(t, nil)

	response := httptest.NewRecorder()
	StatsPage(response, httptest.NewRequest(http.MethodGet, "/statistik", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("StatsPage status = %d, want %d", response.Code, http.StatusOK)
	}
	if !strings.Contains(response.Body.String(), "Statistiken laddas") {
		t.Error("empty catalogue did not render the loading state")
	}
}
