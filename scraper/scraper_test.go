package scraper

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	server "github.com/ebinbellini/ikeadle/server"
)

const searchFixture = `{
  "results": [
    {
      "component": "PRIMARY_AREA",
      "metadata": {"start": 0, "end": 3, "max": 119},
      "items": [
        {
          "type": "PRODUCT",
          "product": {
            "itemNo": "00263850",
            "name": "BILLY",
            "typeName": "Bokhylla",
            "validDesignText": "vit",
            "itemMeasureReferenceText": "80x28x202 cm",
            "mainImageUrl": "https://www.ikea.com/se/sv/images/products/billy-bokhylla-vit__1590272_pe1038911_s5.jpg",
            "pipUrl": "https://www.ikea.com/se/sv/p/billy-bokhylla-vit-00263850/",
            "salesPrice": {"currencyCode": "SEK", "numeral": 699},
            "categoryPath": [
              {"name": "Förvaring", "key": "st001"},
              {"name": "Hyllor & bokhyllor", "key": "st002"},
              {"name": "Bokhyllor", "key": "10382"}
            ]
          }
        },
        {"type": "FOCUS_POINT"},
        {
          "type": "PRODUCT",
          "product": {
            "itemNo": "99999999",
            "name": "UTAN BILD",
            "salesPrice": {"currencyCode": "SEK", "numeral": 99},
            "categoryPath": []
          }
        }
      ]
    }
  ]
}`

func TestSearchResponseParsingAndExtraction(t *testing.T) {
	var response searchResponse
	if err := json.Unmarshal([]byte(searchFixture), &response); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	if len(response.Results) != 1 {
		t.Fatalf("got %d result components, want 1", len(response.Results))
	}
	block := response.Results[0]
	if block.Component != "PRIMARY_AREA" {
		t.Fatalf("component = %q, want PRIMARY_AREA", block.Component)
	}
	if block.Metadata.Max != 119 {
		t.Fatalf("metadata.max = %d, want 119", block.Metadata.Max)
	}
	if len(block.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(block.Items))
	}

	if block.Items[0].Product == nil {
		t.Fatal("first item has no product")
	}
	product, ok := ExtractProduct(*block.Items[0].Product)
	if !ok {
		t.Fatal("ExtractProduct rejected a playable product")
	}

	if product.ID != "00263850" {
		t.Errorf("id = %q, want 00263850", product.ID)
	}
	if product.Name != "BILLY" {
		t.Errorf("name = %q, want BILLY", product.Name)
	}
	if product.Description != "Bokhylla, vit" {
		t.Errorf("description = %q, want %q", product.Description, "Bokhylla, vit")
	}
	if product.Price != 699 {
		t.Errorf("price = %v, want 699", product.Price)
	}
	if product.PriceUnit != "kr" {
		t.Errorf("price unit = %q, want kr", product.PriceUnit)
	}
	if product.Amount != "80x28x202 cm" {
		t.Errorf("amount = %q, want 80x28x202 cm", product.Amount)
	}
	if product.CategoryPath != "Förvaring / Hyllor & bokhyllor / Bokhyllor" {
		t.Errorf("category path = %q", product.CategoryPath)
	}
	if product.URL != "https://www.ikea.com/se/sv/p/billy-bokhylla-vit-00263850/" {
		t.Errorf("url = %q", product.URL)
	}

	if _, ok := ExtractProduct(*block.Items[2].Product); ok {
		t.Error("ExtractProduct accepted a product without an image")
	}
}

func TestExtractProductRejectsUnplayableItems(t *testing.T) {
	base := ikeaProduct{
		ItemNo:       "12345678",
		Name:         "TEST",
		MainImageURL: "https://example.com/test.jpg",
		TypeName:     "Sak",
	}
	base.SalesPrice.CurrencyCode = "SEK"
	base.SalesPrice.Numeral = 49

	tests := []struct {
		name   string
		mutate func(*ikeaProduct)
	}{
		{name: "missing item number", mutate: func(p *ikeaProduct) { p.ItemNo = "" }},
		{name: "missing name", mutate: func(p *ikeaProduct) { p.Name = " " }},
		{name: "missing image", mutate: func(p *ikeaProduct) { p.MainImageURL = "" }},
		{name: "zero price", mutate: func(p *ikeaProduct) { p.SalesPrice.Numeral = 0 }},
		{name: "negative price", mutate: func(p *ikeaProduct) { p.SalesPrice.Numeral = -1 }},
		{name: "wrong currency", mutate: func(p *ikeaProduct) { p.SalesPrice.CurrencyCode = "EUR" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := base
			test.mutate(&item)
			if _, ok := ExtractProduct(item); ok {
				t.Error("ExtractProduct accepted an unplayable item")
			}
		})
	}
}

func TestExtractProductWithoutDesignOrMeasurements(t *testing.T) {
	item := ikeaProduct{ItemNo: "12345678", Name: "TEST", TypeName: "Sak", MainImageURL: "https://example.com/test.jpg"}
	item.SalesPrice.CurrencyCode = "SEK"
	item.SalesPrice.Numeral = 49

	product, ok := ExtractProduct(item)
	if !ok {
		t.Fatal("ExtractProduct rejected a valid product")
	}
	if product.Description != "Sak" {
		t.Errorf("description = %q, want Sak", product.Description)
	}
	if product.CategoryPath != "" {
		t.Errorf("category path = %q, want empty", product.CategoryPath)
	}
}

func TestDiscoverCategories(t *testing.T) {
	const pageFixture = `{
	  "searchResultPage": {
	    "products": {
	      "filters": [
	        {
	          "parameter": "f-subcategories",
	          "values": [
	            {"id": "st001", "name": "Förvaring", "count": 2283},
	            {"id": "de001", "name": "Dekoration & inredning", "count": 1551},
	            {"id": "bc001", "name": "Barnens IKEA", "count": 822}
	          ]
	        }
	      ]
	    }
	  }
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search-result-page" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.URL.Query().Get("size") != "1" {
			t.Errorf("size = %q, want 1", r.URL.Query().Get("size"))
		}
		if !strings.Contains(r.UserAgent(), "IKEAdle") {
			t.Errorf("user agent = %q, want it to identify IKEAdle", r.UserAgent())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(pageFixture))
	}))
	defer server.Close()

	categories, err := discoverCategoriesAt(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("discoverCategoriesAt: %v", err)
	}

	want := []struct {
		id   string
		name string
	}{
		{id: "bc001", name: "Barnens IKEA"},
		{id: "de001", name: "Dekoration & inredning"},
		{id: "st001", name: "Förvaring"},
	}
	if len(categories) != len(want) {
		t.Fatalf("got %d categories, want %d", len(categories), len(want))
	}
	for index, expected := range want {
		if len(categories[index].Slugs) != 1 || categories[index].Slugs[0] != expected.id {
			t.Errorf("category %d slug = %v, want %s", index, categories[index].Slugs, expected.id)
		}
		if len(categories[index].Names) != 1 || categories[index].Names[0] != expected.name {
			t.Errorf("category %d name = %v, want %s", index, categories[index].Names, expected.name)
		}
	}
}

func TestDiscoverCategoriesMissingFilter(t *testing.T) {
	const pageFixture = `{"searchResultPage":{"products":{"filters":[]}}}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(pageFixture))
	}))
	defer server.Close()

	if _, err := discoverCategoriesAt(server.Client(), server.URL); err == nil {
		t.Fatal("expected an error when the category filter is missing")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func testProductJSON(name string) string {
	return `{
	  "type": "PRODUCT",
	  "product": {
	    "itemNo": "` + name + `",
	    "name": "` + name + `",
	    "typeName": "Sak",
	    "itemMeasureReferenceText": "10x10 cm",
	    "mainImageUrl": "https://example.com/` + name + `.jpg",
	    "pipUrl": "https://www.ikea.com/se/sv/p/` + name + `/",
	    "salesPrice": {"currencyCode": "SEK", "numeral": 10},
	    "categoryPath": [{"name": "Förvaring", "key": "st001"}]
	  }
	}`
}

// A page that is shorter than pageSize must advance the offset by the number of
// items actually returned, not by pageSize, or products would be skipped.
func TestFindProductsInCategoryAdvancesByReturnedItems(t *testing.T) {
	var offsets []int

	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body searchRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		offset := body.Components[0].Window.Offset
		offsets = append(offsets, offset)

		response := `{"results":[{"component":"PRIMARY_AREA","metadata":{"max":3},"items":[]}]}`
		if offset < 3 {
			response = `{"results":[{"component":"PRIMARY_AREA","metadata":{"max":3},"items":[` +
				testProductJSON(fmt.Sprintf("0000000%d", offset+1)) + `]}]}`
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(response)),
		}, nil
	})}

	products, err := FindProductsInCategory(client, server.Category{
		Slugs: []string{"st001"},
		Names: []string{"Förvaring"},
	})
	if err != nil {
		t.Fatalf("FindProductsInCategory: %v", err)
	}

	if len(products) != 3 {
		t.Fatalf("got %d products, want 3", len(products))
	}
	for index, product := range products {
		want := fmt.Sprintf("0000000%d", index+1)
		if product.ID != want {
			t.Errorf("product %d id = %q, want %q", index, product.ID, want)
		}
	}

	wantOffsets := []int{0, 1, 2}
	if len(offsets) != len(wantOffsets) {
		t.Fatalf("offsets = %v, want %v", offsets, wantOffsets)
	}
	for index, want := range wantOffsets {
		if offsets[index] != want {
			t.Errorf("request %d offset = %d, want %d", index, offsets[index], want)
		}
	}
}

func testJSONResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestScrapeCatalogueUsesRootCategory(t *testing.T) {
	var requestedInput string

	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body searchRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		requestedInput = body.SearchParameters.Input

		return testJSONResponse(`{"results":[{"component":"PRIMARY_AREA","metadata":{"max":1},"items":[` +
			testProductJSON("00000001") + `]}]}`), nil
	})}

	categories, err := scrapeCatalogue(client)
	if err != nil {
		t.Fatalf("scrapeCatalogue: %v", err)
	}

	if requestedInput != rootCategoryID {
		t.Errorf("requested input = %q, want %q", requestedInput, rootCategoryID)
	}

	category, ok := categories[rootCategoryID]
	if !ok {
		t.Fatalf("category map has no %q key: %v", rootCategoryID, categories)
	}
	if len(category.Products) != 1 {
		t.Fatalf("got %d products, want 1", len(category.Products))
	}
	if category.Products[0].ID != "00000001" {
		t.Errorf("product id = %q, want 00000001", category.Products[0].ID)
	}
}
