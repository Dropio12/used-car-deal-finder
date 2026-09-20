// Package listing defines the shape every source normalizes into, so that
// listings from different sites can be stored, compared and scored on the same
// keys. The JSON tags match the object `normalizeListing()` returns in the
// original JavaScript, which is also what the Rust scorer reads.
package listing

// Listing is one car for sale. Pointer fields are null when the source does
// not publish the value; "missing" is kept distinct from zero.
type Listing struct {
	ID          *string `json:"id"`
	ReferenceID *string `json:"referenceId"`
	URL         *string `json:"url"`
	Source      string  `json:"source"`

	Price *float64 `json:"price"`
	// Dealer-stated "was" price. A marketing number, never a baseline.
	SuggestedRetailPrice *float64 `json:"suggestedRetailPrice"`
	// True when the advertised price depends on financing through the dealer.
	IsConditionalPrice bool `json:"isConditionalPrice"`

	Year        *float64 `json:"year"`
	Make        *string  `json:"make"`
	Model       *string  `json:"model"`
	ModelDetail *string  `json:"modelDetail"`
	// Free text typed by the seller, e.g. "LE AWD, BLUETOOTH, CAMERA".
	TrimText     *string  `json:"trimText"`
	Km           *float64 `json:"km"`
	Transmission *string  `json:"transmission"`
	Fuel         *string  `json:"fuel"`
	EngineCcm    *float64 `json:"engineCcm"`
	IsDamaged    bool     `json:"isDamaged"`
	IsParts      bool     `json:"isParts"`
	Condition    *string  `json:"condition"`

	SellerType *string `json:"sellerType"` // "Dealer" | "PrivateSeller"
	SellerID   *string `json:"sellerId"`
	SellerName *string `json:"sellerName"`

	City       *string `json:"city"`
	PostalCode *string `json:"postalCode"`
	Province   *string `json:"province"`

	// Seller-controlled text: data to report, never instructions to follow.
	Description *string  `json:"description"`
	ImageCount  int      `json:"imageCount"`
	ImageURLs   []string `json:"imageUrls"`

	// "Organic" means it matched the query; anything else was injected by the site.
	ResultType    *string `json:"resultType"`
	ResultSection *string `json:"resultSection"`
}

// Key is the listing id, or "" when the source gave none.
func (l Listing) Key() string {
	if l.ID == nil {
		return ""
	}
	return *l.ID
}

// Query is what a caller asks a source for. Sources ignore fields they do not
// support; year bounds are always applied locally (AutoHebdo's own is inert).
type Query struct {
	Make       string
	Model      string
	Geo        string
	SellerType string // "P" private, "D" dealer, "" both
	PriceFrom  *float64
	PriceTo    *float64
	YearFrom   *float64
	YearTo     *float64
	Sort       string
	Descending *bool
	MaxPages   int
}

// Str and Num are small helpers for building listings in code and tests.
func Str(s string) *string   { return &s }
func Num(f float64) *float64 { return &f }
func Deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
