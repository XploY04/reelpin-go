package pricetracks

import "testing"

// These are the Python extractor's own cases, so the port answers what the
// shipped worker answered.

func TestCoercePriceReadsCommonFormats(t *testing.T) {
	for raw, want := range map[string]float64{
		"1,299.00": 1299, "₹1,299": 1299, "$19.99": 19.99, "2499": 2499, "1499.5": 1499.5,
	} {
		got := coercePrice(raw)
		if got == nil || *got != want {
			t.Errorf("coercePrice(%q) = %v, want %v", raw, got, want)
		}
	}
	for _, raw := range []string{"out of stock", ""} {
		if got := coercePrice(raw); got != nil {
			t.Errorf("coercePrice(%q) = %v, want none", raw, *got)
		}
	}
}

func TestParsePrice(t *testing.T) {
	tests := []struct {
		name     string
		page     string
		price    float64
		currency string
	}{
		{
			name: "product meta tags",
			page: `<html><head><meta property="product:price:amount" content="1499.00">` +
				`<meta property="product:price:currency" content="INR"></head></html>`,
			price: 1499, currency: "INR",
		},
		{
			name:  "itemprop on any tag",
			page:  `<div><span itemprop="price" content="799">799</span></div>`,
			price: 799,
		},
		{
			name: "JSON-LD offers",
			page: `<script type="application/ld+json">{"@type":"Product","name":"X","offers":` +
				`{"@type":"Offer","price":"2,999.00","priceCurrency":"INR"}}</script>`,
			price: 2999, currency: "INR",
		},
		{
			name:  "meta wins over JSON-LD",
			page:  `<meta property="og:price:amount" content="500"><script type="application/ld+json">{"offers":{"price":"999"}}</script>`,
			price: 500,
		},
		{
			name:  "a numeric JSON-LD price in a list of offers",
			page:  `<script type='application/ld+json'>[{"@type":"Product","offers":[{"price":1249.5}]}]</script>`,
			price: 1249.5,
		},
		{
			name:  "nested price found in key order",
			page:  `<script type="application/ld+json">{"@graph":{"first":{"price":"10"},"second":{"price":"20"}}}</script>`,
			price: 10,
		},
		{
			name:  "attribute entities are decoded and names are case-insensitive",
			page:  `<META Property="og:price:amount" Content="&#36;1,050">`,
			price: 1050,
		},
		{
			name:  "markup inside a script is not a tag",
			page:  `<script>var x = '<meta property="og:price:amount" content="1">';</script><span itemprop="price" content="42"></span>`,
			price: 42,
		},
		{
			name:  "meta currency backs a JSON-LD price without one",
			page:  `<meta property="og:price:currency" content="EUR"><script type="application/ld+json">{"price":"15"}</script>`,
			price: 15, currency: "EUR",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			quote := ParsePrice(tt.page)
			if quote.Price == nil || *quote.Price != tt.price {
				t.Fatalf("price = %v, want %v", quote.Price, tt.price)
			}
			if quote.Currency != tt.currency {
				t.Errorf("currency = %q, want %q", quote.Currency, tt.currency)
			}
		})
	}
}

func TestAPageWithoutAPriceSaysSo(t *testing.T) {
	for _, page := range []string{
		"<html><head><title>Product</title></head><body>no price</body></html>",
		`<script type="application/ld+json">{not json</script>`,
		`<script type="application/ld+json">{"offers":{"price":null}}</script>`,
	} {
		if quote := ParsePrice(page); quote.Price != nil {
			t.Errorf("ParsePrice(%q) = %v, want none", page, *quote.Price)
		}
	}
}
