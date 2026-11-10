package scraper

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	server "github.com/ebinbellini/ikeadle/server"
)

// IKEA's product listing is driven by Ingka's SIK search API. The public
// website itself calls this API from the browser, which makes it a much better
// scraping target than the HTML pages:
//
//   - one JSON request returns up to hundreds of products, each with name,
//     type, design, measurements, price, image and category path;
//   - pagination is an offset in the request body, so a whole category can be
//     read without following "load more" links;
//   - the root category "products" covers the whole Swedish range, so one
//     paginated query is enough;
//   - the API host's robots.txt allows everything, and the site's own listing
//     pages are rendered from the very same responses.
//
// A full scrape of the Swedish range (~10 000 products) is therefore about
// twenty requests instead of one request per product or per HTML page.
const (
	ikeaBaseURL    = "https://sik.search.blue.cdtapps.com/se/sv"
	ikeaSearchPath = "/search"
	ikeaPagePath   = "/search-result-page"

	// rootCategoryID is the id of IKEA's "Produkter" node. Asking the search
	// API for this one category returns the entire Swedish range, so the
	// scraper can page through one query instead of every top-level category.
	rootCategoryID   = "products"
	rootCategoryName = "Alla varor"
	rootCategoryURL  = "https://www.ikea.com/se/sv/cat/produkter-products/"

	// ikeaAPIVersion is the SIK contract version the website is currently
	// using. IKEA keeps older versions working for a long time, but the
	// version can be overridden with IKEA_API_VERSION without a rebuild.
	ikeaAPIVersion = "20260727"
	ikeaAPIChannel = "listaf"

	// pageSize is how many products are requested per POST. The API happily
	// returns more, but a few hundred keeps each response at a few megabytes
	// and makes a failed page cheap to retry.
	pageSize = 500

	// maxCategoryPages is a safety net against broken pagination. The full
	// catalogue currently needs twenty-one pages at pageSize.
	maxCategoryPages = 100

	categoryCacheFile = "/tmp/ikeadle_category_map_cache.bin"

	// categoryFilterParameter is the filter in the search response that lists
	// the top-level categories. It is how we discover the catalogue without
	// hard-coding category ids.
	categoryFilterParameter = "f-subcategories"

	// userAgent identifies the crawler and gives IKEA a way to contact the
	// project if the request volume ever becomes a problem.
	userAgent = "IKEAdle/1.0 (+https://github.com/ebinbellini/ikeadle) price guessing fan game"
)

// ikeaProduct mirrors the product fields IKEAdle needs from a SIK search item.
type ikeaProduct struct {
	ItemNo                   string `json:"itemNo"`
	Name                     string `json:"name"`
	TypeName                 string `json:"typeName"`
	ValidDesignText          string `json:"validDesignText"`
	ItemMeasureReferenceText string `json:"itemMeasureReferenceText"`
	MainImageURL             string `json:"mainImageUrl"`
	PipURL                   string `json:"pipUrl"`
	SalesPrice               struct {
		CurrencyCode string  `json:"currencyCode"`
		Numeral      float64 `json:"numeral"`
	} `json:"salesPrice"`
	CategoryPath []struct {
		Name string `json:"name"`
		Key  string `json:"key"`
	} `json:"categoryPath"`
}

// searchItem is one entry in the product list. IKEA mixes products, promotional
// tiles and focus points in the same array; only entries with a product are
// useful here.
type searchItem struct {
	Type    string       `json:"type"`
	Product *ikeaProduct `json:"product"`
}

// searchBlock is the PRIMARY_AREA component of a search response: the items
// plus the pagination window.
type searchBlock struct {
	Items    []searchItem `json:"items"`
	Metadata struct {
		Start int `json:"start"`
		End   int `json:"end"`
		Max   int `json:"max"`
	} `json:"metadata"`
}

// searchResponse is the POST /search response.
type searchResponse struct {
	Results []struct {
		Component string `json:"component"`
		searchBlock
	} `json:"results"`
}

// searchPageResponse is the GET /search-result-page response. It is used once
// per scrape to discover the top-level categories from the filter list.
type searchPageResponse struct {
	SearchResultPage struct {
		Products struct {
			Main    searchBlock      `json:"main"`
			Filters []categoryFilter `json:"filters"`
		} `json:"products"`
	} `json:"searchResultPage"`
}

type categoryFilter struct {
	Parameter string `json:"parameter"`
	Values    []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Count int    `json:"count"`
	} `json:"values"`
}

// searchRequest is the body of POST /search. It is deliberately small: the
// listing component only asks for products and a window, no filters, sorting or
// availability data.
type searchRequest struct {
	SearchParameters struct {
		Input string `json:"input"`
		Type  string `json:"type"`
	} `json:"searchParameters"`
	IsUserLoggedIn bool              `json:"isUserLoggedIn"`
	IsB2B          bool              `json:"isB2B"`
	Components     []searchComponent `json:"components"`
}

type searchComponent struct {
	Component string `json:"component"`
	Columns   int    `json:"columns"`
	Types     struct {
		Main      string   `json:"main"`
		Breakouts []string `json:"breakouts"`
	} `json:"types"`
	Window struct {
		Offset int `json:"offset"`
		Size   int `json:"size"`
	} `json:"window"`
}

func apiVersion() string {
	if version := strings.TrimSpace(os.Getenv("IKEA_API_VERSION")); version != "" {
		return version
	}
	return ikeaAPIVersion
}

// DiscoverCategories reads the top-level category list from the filter that
// every product search response contains. It is the fallback path for the rare
// case where the root category cannot be read: no category tree endpoint or
// HTML navigation page is needed either way.
func DiscoverCategories(hc *http.Client) ([]server.Category, error) {
	return discoverCategoriesAt(hc, ikeaBaseURL)
}

func discoverCategoriesAt(hc *http.Client, baseURL string) ([]server.Category, error) {
	url := fmt.Sprintf("%s%s?size=1&c=%s&v=%s&types=PRODUCT",
		baseURL, ikeaPagePath, ikeaAPIChannel, apiVersion())

	request, err := newRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create category request: %w", err)
	}

	var page searchPageResponse
	if err := fetchJSON(hc, request, &page); err != nil {
		return nil, fmt.Errorf("discover categories: %w", err)
	}

	for _, filter := range page.SearchResultPage.Products.Filters {
		if filter.Parameter != categoryFilterParameter {
			continue
		}

		categories := make([]server.Category, 0, len(filter.Values))
		for _, value := range filter.Values {
			if value.ID == "" || value.Name == "" {
				continue
			}
			categories = append(categories, server.Category{
				Slugs: []string{value.ID},
				Names: []string{value.Name},
				URL:   "https://www.ikea.com/se/sv/cat/",
			})
		}

		if len(categories) == 0 {
			break
		}

		sort.Slice(categories, func(i, j int) bool {
			return categories[i].Names[0] < categories[j].Names[0]
		})
		return categories, nil
	}

	return nil, fmt.Errorf("the %q filter is missing from the search response", categoryFilterParameter)
}

// FetchCategoryPage requests one window of products in a category.
func FetchCategoryPage(hc *http.Client, categoryID string, offset int) (*searchBlock, error) {
	body := searchRequest{IsUserLoggedIn: false, IsB2B: false}
	body.SearchParameters.Input = categoryID
	body.SearchParameters.Type = "CATEGORY"

	component := searchComponent{Component: "PRIMARY_AREA", Columns: 4}
	component.Types.Main = "PRODUCT"
	component.Types.Breakouts = []string{}
	component.Window.Offset = offset
	component.Window.Size = pageSize
	body.Components = []searchComponent{component}

	url := fmt.Sprintf("%s%s?c=%s&v=%s", ikeaBaseURL, ikeaSearchPath, ikeaAPIChannel, apiVersion())

	request, err := newRequest(http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("create product request: %w", err)
	}

	var response searchResponse
	if err := fetchJSON(hc, request, &response); err != nil {
		return nil, fmt.Errorf("fetch category %s at offset %d: %w", categoryID, offset, err)
	}

	for index := range response.Results {
		if response.Results[index].Component == "PRIMARY_AREA" {
			return &response.Results[index].searchBlock, nil
		}
	}
	if len(response.Results) > 0 {
		return &response.Results[0].searchBlock, nil
	}

	return nil, fmt.Errorf("category %s returned no product component", categoryID)
}

// ExtractProduct converts one SIK product into the game's product model.
// Products without a usable price, image or name are skipped: they cannot be
// played and would only bloat the pool.
func ExtractProduct(item ikeaProduct) (server.Product, bool) {
	if item.ItemNo == "" || strings.TrimSpace(item.Name) == "" {
		return server.Product{}, false
	}
	if strings.TrimSpace(item.MainImageURL) == "" {
		return server.Product{}, false
	}
	if item.SalesPrice.CurrencyCode != "" && item.SalesPrice.CurrencyCode != "SEK" {
		return server.Product{}, false
	}
	if item.SalesPrice.Numeral <= 0 {
		return server.Product{}, false
	}

	description := strings.TrimSpace(item.TypeName)
	if design := strings.TrimSpace(item.ValidDesignText); design != "" {
		if description == "" {
			description = design
		} else {
			description += ", " + design
		}
	}

	path := make([]string, 0, len(item.CategoryPath))
	for _, category := range item.CategoryPath {
		if name := strings.TrimSpace(category.Name); name != "" {
			path = append(path, name)
		}
	}

	return server.Product{
		ID:           item.ItemNo,
		Name:         strings.TrimSpace(item.Name),
		Description:  description,
		URL:          item.PipURL,
		ImageURL:     item.MainImageURL,
		Price:        item.SalesPrice.Numeral,
		PriceUnit:    "kr",
		Amount:       strings.TrimSpace(item.ItemMeasureReferenceText),
		CategoryPath: strings.Join(path, " / "),
		ScrapedAt:    time.Now(),
	}, true
}

// FindProductsInCategory reads every page of a category. A failed page after
// the first one keeps the products already collected instead of dropping the
// whole category.
func FindProductsInCategory(hc *http.Client, category server.Category) ([]server.Product, error) {
	if len(category.Slugs) == 0 {
		return nil, fmt.Errorf("category has no id")
	}
	categoryID := strings.Join(category.Slugs, "/")

	products := make([]server.Product, 0, pageSize)
	seen := make(map[string]bool, pageSize)

	offset := 0
	for page := 0; page < maxCategoryPages; page++ {
		block, err := FetchCategoryPage(hc, categoryID, offset)
		if err != nil {
			if page > 0 {
				fmt.Printf("Page %d of %s failed, keeping %d products: %v\n",
					page, categoryID, len(products), err)
				break
			}
			return nil, err
		}

		for _, item := range block.Items {
			if item.Product == nil {
				continue
			}
			product, ok := ExtractProduct(*item.Product)
			if !ok {
				continue
			}
			if seen[product.ID] {
				continue
			}
			seen[product.ID] = true
			products = append(products, product)
		}

		// metadata.max is the number of products in the category. An empty page
		// guards against the field being missing or wrong. Advance by the number
		// of items actually returned, so a short page cannot skip products.
		if len(block.Items) == 0 {
			break
		}
		offset += len(block.Items)
		if block.Metadata.Max > 0 && offset >= block.Metadata.Max {
			break
		}
	}

	fmt.Printf("Found %d products in category: %s\n", len(products), category.Names[0])
	return products, nil
}

// Scrape replaces the whole product pool. It first asks for IKEA's root
// category, which returns the entire range in one paginated query. If that
// fails, it falls back to one query per top-level category. A scrape where
// every query fails leaves the previous pool and cache alone.
func Scrape() error {
	scrapeMu.Lock()
	defer scrapeMu.Unlock()

	delay := crawlDelay()
	fmt.Printf("Crawling IKEA's search API with a %s delay\n", delay)

	client := newCrawlClient(60*time.Second, delay)

	categoryMap, err := scrapeCatalogue(client)
	if err != nil {
		return err
	}

	// Rebuilding the category map also flattens every product into the pool
	// that the daily product is picked from.
	server.SetCategoryMap(categoryMap)

	if err := writeCategoryCache(categoryMap); err != nil {
		return fmt.Errorf("error writing category cache: %w", err)
	}

	return nil
}

// scrapeCatalogue prefers the single root category. The fallback exists so a
// renamed root id cannot take the game down: the top-level filter discovery
// finds the categories without hard-coding them.
func scrapeCatalogue(client *http.Client) (map[string]server.Category, error) {
	root := server.Category{
		Slugs: []string{rootCategoryID},
		Names: []string{rootCategoryName},
		URL:   rootCategoryURL,
	}

	fmt.Printf("Scraping the full IKEA catalogue (%s)\n", rootCategoryID)
	products, err := FindProductsInCategory(client, root)
	if err == nil && len(products) > 0 {
		root.Products = products
		return map[string]server.Category{rootCategoryID: root}, nil
	}

	if err != nil {
		fmt.Printf("Full catalogue scrape failed (%v), falling back to top-level categories\n", err)
	} else {
		fmt.Println("Full catalogue returned no products, falling back to top-level categories")
	}

	return scrapeTopLevelCategories(client)
}

func scrapeTopLevelCategories(client *http.Client) (map[string]server.Category, error) {
	categories, err := DiscoverCategories(client)
	if err != nil {
		return nil, fmt.Errorf("error discovering categories: %w", err)
	}

	categoryMap := make(map[string]server.Category, len(categories))
	failed := 0

	for _, category := range categories {
		fmt.Printf("Scraping category: %s (%s)\n", category.Names[0], category.Slugs[0])
		products, err := FindProductsInCategory(client, category)
		if err != nil {
			failed++
			fmt.Printf("Skipping category %s: %v\n", category.Names[0], err)
			continue
		}
		if len(products) == 0 {
			continue
		}
		category.Products = products
		categoryMap[category.Slugs[0]] = category
	}

	if len(categoryMap) == 0 {
		return nil, fmt.Errorf("every category failed to scrape (%d tried)", failed)
	}
	if failed > 0 {
		fmt.Printf("%d of %d categories failed, keeping the %d that worked\n",
			failed, len(categories), len(categoryMap))
	}

	return categoryMap, nil
}

// scrapeMu serializes startup and scheduled scrapes. Without it two runs could
// overlap and issue requests closer together than the crawl delay.
var scrapeMu sync.Mutex

func writeCategoryCache(categoryMap map[string]server.Category) error {
	dir := filepath.Dir(categoryCacheFile)
	file, err := os.CreateTemp(dir, "ikeadle-cache-*.tmp")
	if err != nil {
		return err
	}
	tempName := file.Name()

	cleanup := func() {
		file.Close()
		os.Remove(tempName)
	}

	if err := gob.NewEncoder(file).Encode(categoryMap); err != nil {
		cleanup()
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(tempName)
		return err
	}

	// Rename is atomic, so a killed process can never leave a half-written
	// cache behind.
	return os.Rename(tempName, categoryCacheFile)
}

// readCategoryCache loads the category map from disk.
func readCategoryCache() (map[string]server.Category, error) {
	file, err := os.Open(categoryCacheFile)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	categoryMap := make(map[string]server.Category)
	if err := gob.NewDecoder(file).Decode(&categoryMap); err != nil {
		return nil, err
	}
	if len(categoryMap) == 0 {
		return nil, fmt.Errorf("cache holds no categories")
	}

	return categoryMap, nil
}

// LoadOrScrape reuses the cached catalogue when there is one, so restarts do
// not hammer IKEA. A missing or unreadable cache triggers a full scrape.
func LoadOrScrape() {
	categoryMap, err := readCategoryCache()
	if err == nil {
		server.SetCategoryMap(categoryMap)
		fmt.Printf("Loaded category map from disk (%d categories)\n", len(categoryMap))
		return
	}

	fmt.Println("No usable category map on disk, scraping IKEA... (" + err.Error() + ")")
	if err := Scrape(); err != nil {
		fmt.Println("Error scraping IKEA:", err)
	}
}

func newRequest(method, url string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, err
	}

	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", userAgent)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	return request, nil
}

// fetchJSON performs a request and decodes the response. The body is capped so
// a broken or hostile response cannot exhaust memory.
func fetchJSON(hc *http.Client, request *http.Request, target any) error {
	response, err := hc.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("unexpected status %s: %s", response.Status, strings.TrimSpace(string(detail)))
	}

	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<20))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	return nil
}
