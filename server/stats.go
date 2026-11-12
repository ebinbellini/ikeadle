package server

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The statistics page has two halves. The catalogue numbers are derived from
// product data only. The guess numbers are anonymous aggregates from the local
// SQLite log: no names, accounts, IP addresses or other identifiers are stored.
// Everything catalogue-related comes from the same playable product pool that
// the game modes use, so the numbers cannot disagree with the game about which
// products exist.

const nbsp = "\u00a0"

// priceBucketDef is one interval in the price histogram. Max is exclusive, and
// Max == 0 means "no upper bound" for the last bucket.
type priceBucketDef struct {
	Min float64
	Max float64
}

// priceBucketDefinitions is deliberately denser at the lower end, where most
// of the catalogue lives, and coarser among the expensive products. The price
// spans roughly 5-50 000 kr, so linear buckets would put almost everything in
// the first one or two bars.
var priceBucketDefinitions = []priceBucketDef{
	{Min: 0, Max: 50},
	{Min: 50, Max: 100},
	{Min: 100, Max: 200},
	{Min: 200, Max: 300},
	{Min: 300, Max: 500},
	{Min: 500, Max: 750},
	{Min: 750, Max: 1000},
	{Min: 1000, Max: 1500},
	{Min: 1500, Max: 2000},
	{Min: 2000, Max: 3000},
	{Min: 3000, Max: 5000},
	{Min: 5000, Max: 10000},
	{Min: 10000, Max: 0},
}

// statsSummary holds the four headline numbers at the top of the page.
type statsSummary struct {
	ProductCount  string
	CategoryCount string
	MedianPrice   string
	PriceRange    string
}

// priceBucket is one rendered column of the histogram.
type priceBucket struct {
	Label       string
	Count       int
	CountText   string
	ShareText   string
	BarPercent  float64
	IsMedian    bool
	HasProducts bool
}

// categoryStat is one top-level category in the category price chart.
type categoryStat struct {
	Name        string
	Count       int
	CountText   string
	MedianPrice float64
	MedianText  string
	RangeText   string
	BarPercent  float64
	HasProducts bool
}

// scaleTick is one label on the logarithmic category price scale. Position is
// a percentage from the left edge of the chart, so the label lines up with the
// price it represents instead of being spaced by its own text width.
type scaleTick struct {
	Label    string
	Position float64
}

// statsPageData feeds templates/pages/statistik.html.
type statsPageData struct {
	pageData

	Empty        bool
	Summary      statsSummary
	Buckets      []priceBucket
	MedianBucket string
	Categories   []categoryStat
	ScaleTicks   []scaleTick
	Updated      string
	GuessStats   guessStatsView
}

// StatsPage renders the catalogue statistics. It is a page, not a game mode,
// and needs no JavaScript: all numbers are calculated when the request arrives.
func StatsPage(w http.ResponseWriter, r *http.Request) {
	data := buildStats(productSnapshot())
	data.GuessStats = loadGuessStats()
	data.pageData = pageData{
		Title:      "IKEAdle - Statistik",
		ActiveLink: "/statistik",
		Modes:      modes,
	}

	renderPage(w, statsPage, data)
}

// buildStats turns a product snapshot into everything the statistics page
// shows. It is a pure function so the calculations can be tested without HTTP
// or global state.
func buildStats(products []Product) statsPageData {
	data := statsPageData{}

	prices := make([]float64, 0, len(products))
	categoryPrices := make(map[string][]float64)
	var lowest, highest float64
	var latestScrape time.Time

	for _, product := range products {
		if product.Price <= 0 {
			continue
		}

		price := product.Price
		prices = append(prices, price)

		if len(prices) == 1 || price < lowest {
			lowest = price
		}
		if len(prices) == 1 || price > highest {
			highest = price
		}

		category := topLevelCategory(product.CategoryPath)
		categoryPrices[category] = append(categoryPrices[category], price)

		if product.ScrapedAt.After(latestScrape) {
			latestScrape = product.ScrapedAt
		}
	}

	total := len(prices)
	if total == 0 {
		data.Empty = true
		return data
	}

	catalogueMedian := medianPrice(prices)
	// The headline price is shown in whole kronor, so use the same rounded
	// value when telling the reader which histogram bucket it falls in.
	displayMedian := math.Round(catalogueMedian)
	data.Summary = statsSummary{
		ProductCount:  formatNumber(total),
		CategoryCount: formatNumber(len(categoryPrices)),
		MedianPrice:   formatSEK(displayMedian),
		PriceRange:    formatPriceRange(lowest, highest),
	}

	if !latestScrape.IsZero() {
		data.Updated = latestScrape.Format("2006-01-02")
	}

	counts := make([]int, len(priceBucketDefinitions))
	for _, price := range prices {
		for index, bucket := range priceBucketDefinitions {
			if priceInBucket(price, bucket) {
				counts[index]++
				break
			}
		}
	}

	maxCount := 0
	medianBucket := ""
	for index, count := range counts {
		if count > maxCount {
			maxCount = count
		}
		if priceInBucket(displayMedian, priceBucketDefinitions[index]) {
			medianBucket = bucketPriceLabel(priceBucketDefinitions[index])
		}
	}

	data.MedianBucket = medianBucket
	data.Buckets = make([]priceBucket, 0, len(counts))
	for index, count := range counts {
		bucket := priceBucket{
			Label:       bucketPriceLabel(priceBucketDefinitions[index]),
			Count:       count,
			CountText:   formatNumber(count),
			ShareText:   formatShare(count, total),
			IsMedian:    bucketPriceLabel(priceBucketDefinitions[index]) == medianBucket,
			HasProducts: count > 0,
		}
		if maxCount > 0 {
			bucket.BarPercent = roundedPercent(float64(count) / float64(maxCount) * 100)
		}
		data.Buckets = append(data.Buckets, bucket)
	}

	data.Categories = buildCategoryStats(categoryPrices)
	data.ScaleTicks = priceLogScale(data.Categories)
	return data
}

// buildCategoryStats calculates the median and the price range for every
// top-level category and places each median on a shared logarithmic scale.
func buildCategoryStats(categoryPrices map[string][]float64) []categoryStat {
	categories := make([]categoryStat, 0, len(categoryPrices))

	for name, prices := range categoryPrices {
		median := medianPrice(prices)
		lowest, highest := priceExtremes(prices)
		categories = append(categories, categoryStat{
			Name:        name,
			Count:       len(prices),
			CountText:   formatNumber(len(prices)),
			MedianPrice: median,
			MedianText:  formatSEK(median),
			RangeText:   formatPriceRange(lowest, highest),
			HasProducts: len(prices) > 0,
		})
	}

	sort.Slice(categories, func(i, j int) bool {
		if categories[i].MedianPrice != categories[j].MedianPrice {
			return categories[i].MedianPrice > categories[j].MedianPrice
		}
		if categories[i].Count != categories[j].Count {
			return categories[i].Count > categories[j].Count
		}
		return categories[i].Name < categories[j].Name
	})

	if len(categories) == 0 {
		return categories
	}

	// The median prices usually span one or two powers of ten, so a log scale
	// keeps both budget and premium categories readable. The domain is rounded
	// out to whole decades, which puts every tick at an exact percentage
	// position on the chart.
	domainLow, domainHigh := priceLogDomain(
		categories[len(categories)-1].MedianPrice,
		categories[0].MedianPrice,
	)
	for index := range categories {
		categories[index].BarPercent = logPricePercent(
			categories[index].MedianPrice,
			domainLow,
			domainHigh,
		)
	}

	return categories
}

func priceLogScale(categories []categoryStat) []scaleTick {
	if len(categories) == 0 {
		return nil
	}

	domainLow, domainHigh := priceLogDomain(
		categories[len(categories)-1].MedianPrice,
		categories[0].MedianPrice,
	)

	ticks := make([]scaleTick, 0, 6)
	for value := domainLow; value <= domainHigh*1.000001; value *= 10 {
		ticks = append(ticks, scaleTick{
			Label:    formatNumber(int(math.Round(value))) + nbsp + "kr",
			Position: logPricePercent(value, domainLow, domainHigh),
		})
	}
	return ticks
}

func priceInBucket(price float64, bucket priceBucketDef) bool {
	if price < bucket.Min {
		return false
	}
	if bucket.Max == 0 {
		return true
	}
	return price < bucket.Max
}

// bucketPriceLabel spells out the range the way a price tag would, e.g.
// "100–199" or "10 000+". The caller adds the unit.
func bucketPriceLabel(bucket priceBucketDef) string {
	if bucket.Max == 0 {
		return formatNumber(int(bucket.Min)) + "+"
	}
	return formatNumber(int(bucket.Min)) + "–" + formatNumber(int(bucket.Max)-1)
}

// topLevelCategory takes the first breadcrumb segment, e.g. "Förvaring" from
// "Förvaring / Hyllor & bokhyllor / Bokhyllor".
func topLevelCategory(categoryPath string) string {
	name := categoryPath
	if separator := strings.Index(name, " / "); separator >= 0 {
		name = name[:separator]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "Övrigt"
	}
	return name
}

func medianPrice(prices []float64) float64 {
	if len(prices) == 0 {
		return 0
	}

	sorted := append([]float64(nil), prices...)
	sort.Float64s(sorted)

	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

func priceExtremes(prices []float64) (float64, float64) {
	if len(prices) == 0 {
		return 0, 0
	}

	lowest, highest := prices[0], prices[0]
	for _, price := range prices[1:] {
		if price < lowest {
			lowest = price
		}
		if price > highest {
			highest = price
		}
	}
	return lowest, highest
}

// priceLogDomain rounds a price range outwards to whole powers of ten.
func priceLogDomain(lowest, highest float64) (float64, float64) {
	if lowest <= 0 {
		lowest = 1
	}
	if highest < lowest {
		highest = lowest
	}

	domainLow := math.Pow(10, math.Floor(math.Log10(lowest)))
	domainHigh := math.Pow(10, math.Ceil(math.Log10(highest)))

	if domainLow < 1 {
		domainLow = 1
	}
	if domainHigh <= domainLow {
		domainHigh = domainLow * 10
	}
	return domainLow, domainHigh
}

func logPricePercent(price, domainLow, domainHigh float64) float64 {
	if price <= 0 || domainLow <= 0 || domainHigh <= domainLow {
		return 0
	}

	span := math.Log10(domainHigh) - math.Log10(domainLow)
	position := (math.Log10(price) - math.Log10(domainLow)) / span * 100
	return roundedPercent(math.Max(0, math.Min(100, position)))
}

func roundedPercent(value float64) float64 {
	return math.Round(value*10) / 10
}

// formatNumber groups thousands with a non-breaking space, as Swedish prices
// and quantities are normally written.
func formatNumber(value int) string {
	if value < 0 {
		return "-" + formatNumber(-value)
	}

	digits := strconv.Itoa(value)
	var formatted strings.Builder
	for index, digit := range digits {
		if index > 0 && (len(digits)-index)%3 == 0 {
			formatted.WriteString(nbsp)
		}
		formatted.WriteRune(digit)
	}
	return formatted.String()
}

func formatSEK(price float64) string {
	return formatNumber(int(math.Round(price))) + nbsp + "kr"
}

func formatPriceRange(lowest, highest float64) string {
	return formatNumber(int(math.Round(lowest))) + "–" +
		formatNumber(int(math.Round(highest))) + nbsp + "kr"
}

func formatShare(count, total int) string {
	if total <= 0 {
		return "0" + nbsp + "%"
	}

	share := float64(count) / float64(total) * 100
	if count > 0 && share < 0.5 {
		return "<1" + nbsp + "%"
	}
	return strconv.Itoa(int(math.Round(share))) + nbsp + "%"
}
