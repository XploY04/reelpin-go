package pricetracks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"

	"github.com/XploY04/reelpin-go/internal/safehttp"
)

// PageFetcher reads a product page through the SSRF-guarded client, the same
// one every user-supplied URL in this service goes through.
type PageFetcher struct {
	Client *safehttp.Client
}

// FetchPrice answers an empty quote, not an error, for a page that loaded but
// is not HTML or did not succeed: neither is something a retry would change.
func (f PageFetcher) FetchPrice(ctx context.Context, url string) (Quote, error) {
	response, err := f.Client.Get(ctx, url)
	if err != nil {
		return Quote{}, err
	}
	if response.Status < 200 || response.Status > 299 {
		return Quote{}, nil
	}
	if !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "html") {
		return Quote{}, nil
	}
	return ParsePrice(string(response.Body)), nil
}

var (
	startTag   = regexp.MustCompile(`(?is)<([a-z][a-z0-9]*)\b([^>]*)>`)
	attribute  = regexp.MustCompile(`(?s)([^\s"'>/=]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?`)
	scriptTag  = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)
	priceDigit = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)
)

var (
	metaPriceKeys    = map[string]bool{"og:price:amount": true, "product:price:amount": true}
	metaCurrencyKeys = map[string]bool{"og:price:currency": true, "product:price:currency": true}
)

// ParsePrice reads a price from a page's structured data, in the order the
// Python extractor did: Open Graph and product meta tags or an itemprop=price
// attribute, whichever comes first in the page, then JSON-LD offers.
func ParsePrice(page string) Quote {
	metaPrice, metaCurrency := "", ""
	// Tags inside a script are not markup, so they are blanked before
	// scanning; the JSON-LD blocks are read from the original.
	markup := scriptTag.ReplaceAllString(page, "")
	for _, match := range startTag.FindAllStringSubmatch(markup, -1) {
		name := strings.ToLower(match[1])
		attrs := attributes(match[2])
		content := strings.TrimSpace(attrs["content"])
		if name == "meta" && content != "" {
			key := attrs["property"]
			if key == "" {
				key = attrs["name"]
			}
			key = strings.ToLower(key)
			switch {
			case metaPrice == "" && metaPriceKeys[key]:
				metaPrice = content
			case metaCurrency == "" && metaCurrencyKeys[key]:
				metaCurrency = content
			}
		}
		if metaPrice == "" && content != "" && strings.ToLower(attrs["itemprop"]) == "price" {
			metaPrice = content
		}
	}

	if price := coercePrice(metaPrice); price != nil {
		return Quote{Price: price, Currency: metaCurrency}
	}

	for _, match := range scriptTag.FindAllStringSubmatch(page, -1) {
		if strings.ToLower(strings.TrimSpace(attributes(match[1])["type"])) != "application/ld+json" {
			continue
		}
		block, err := decodeOrdered([]byte(strings.TrimSpace(match[2])))
		if err != nil {
			continue
		}
		rawPrice, currency, found := priceFromJSONLD(block)
		if !found {
			continue
		}
		if price := coercePrice(rawPrice); price != nil {
			if currency == "" {
				currency = metaCurrency
			}
			return Quote{Price: price, Currency: currency}
		}
	}
	return Quote{Currency: metaCurrency}
}

// attributes reads one tag's attributes, lowercasing names and decoding
// entities in values as an HTML parser would. The first occurrence wins.
func attributes(raw string) map[string]string {
	values := map[string]string{}
	for _, match := range attribute.FindAllStringSubmatch(raw, -1) {
		name := strings.ToLower(match[1])
		if _, seen := values[name]; seen {
			continue
		}
		values[name] = html.UnescapeString(match[2] + match[3] + match[4])
	}
	return values
}

// coercePrice reads "1,299.00", "₹1,299" or "$19.99" by dropping thousands
// commas and taking the first number. A decimal comma is not handled, as it
// was not in Python.
func coercePrice(value string) *float64 {
	match := priceDigit.FindString(value)
	if match == "" {
		return nil
	}
	price, err := strconv.ParseFloat(strings.ReplaceAll(match, ",", ""), 64)
	if err != nil {
		return nil
	}
	return &price
}

// node is decoded JSON that keeps object key order. The Python extractor
// walked a dict in insertion order, and which nested price wins depends on it.
type node struct {
	object []member
	array  []node
	// scalar is a number's literal text, a string's value, or true/false.
	scalar   string
	isString bool
	isNull   bool
	isObject bool
	isArray  bool
}

type member struct {
	key   string
	value node
}

func (n node) get(key string) (node, bool) {
	for _, m := range n.object {
		if m.key == key {
			return m.value, true
		}
	}
	return node{}, false
}

// truthy is Python's truth value for a decoded JSON value.
func (n node) truthy() bool {
	switch {
	case n.isNull:
		return false
	case n.isObject:
		return len(n.object) > 0
	case n.isArray:
		return len(n.array) > 0
	case n.isString:
		return n.scalar != ""
	}
	value, err := strconv.ParseFloat(n.scalar, 64)
	if err != nil {
		return n.scalar == "true"
	}
	return value != 0
}

// priceFromJSONLD is the Python walk: an offer's price, then the node's own
// price, then the first nested value that has one.
func priceFromJSONLD(n node) (string, string, bool) {
	switch {
	case n.isObject:
		if offers, ok := n.get("offers"); ok {
			candidates := []node{}
			switch {
			case offers.isArray:
				candidates = offers.array
			case offers.truthy():
				candidates = []node{offers}
			}
			for _, offer := range candidates {
				if !offer.isObject {
					continue
				}
				if price, ok := offer.get("price"); ok && !price.isNull {
					return price.scalar, currencyOf(offer), true
				}
			}
		}
		if price, ok := n.get("price"); ok && !price.isNull {
			return price.scalar, currencyOf(n), true
		}
		for _, m := range n.object {
			if price, currency, found := priceFromJSONLD(m.value); found {
				return price, currency, true
			}
		}
	case n.isArray:
		for _, value := range n.array {
			if price, currency, found := priceFromJSONLD(value); found {
				return price, currency, true
			}
		}
	}
	return "", "", false
}

func currencyOf(n node) string {
	currency, ok := n.get("priceCurrency")
	if !ok || !currency.isString {
		return ""
	}
	return currency.scalar
}

func decodeOrdered(raw []byte) (node, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeNode(decoder)
	if err != nil {
		return node{}, err
	}
	if _, err := decoder.Token(); err == nil {
		return node{}, errors.New("trailing data after the JSON value")
	}
	return value, nil
}

func decodeNode(decoder *json.Decoder) (node, error) {
	token, err := decoder.Token()
	if err != nil {
		return node{}, err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			object := node{isObject: true}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return node{}, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return node{}, fmt.Errorf("object key is %T", keyToken)
				}
				child, err := decodeNode(decoder)
				if err != nil {
					return node{}, err
				}
				object.object = append(object.object, member{key: key, value: child})
			}
			_, err := decoder.Token()
			return object, err
		case '[':
			array := node{isArray: true}
			for decoder.More() {
				child, err := decodeNode(decoder)
				if err != nil {
					return node{}, err
				}
				array.array = append(array.array, child)
			}
			_, err := decoder.Token()
			return array, err
		}
		return node{}, fmt.Errorf("unexpected delimiter %v", value)
	case string:
		return node{scalar: value, isString: true}, nil
	case json.Number:
		return node{scalar: value.String()}, nil
	case bool:
		if value {
			return node{scalar: "true"}, nil
		}
		return node{scalar: "false"}, nil
	case nil:
		return node{isNull: true}, nil
	}
	return node{}, fmt.Errorf("unexpected token %T", token)
}
