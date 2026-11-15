package provider

import "fmt"

// PriceCapError means no launch was tried because every current Spot price
// is above the runner's max_price: waiting won't fix it (prices rarely fall
// that far within minutes), only a higher max_price or a running VM will.
type PriceCapError struct {
	Where    string    // the regions searched, for messages
	MaxPrice string    // the runner's max_price
	Cheapest SpotPrice // the cheapest current option
}

// cheapestOf is the lowest price in ps, whatever their order (zero if
// there are none).
func cheapestOf(ps []SpotPrice) SpotPrice {
	var c SpotPrice
	for i, p := range ps {
		if i == 0 || p.USD() < c.USD() {
			c = p
		}
	}
	return c
}

func (e *PriceCapError) Error() string {
	return fmt.Sprintf("every Spot price in %s is above max_price %s (cheapest: $%s/h for %s in %s); raise max_price, or remove it to cap at the on-demand price",
		e.Where, e.MaxPrice, e.Cheapest.Price, e.Cheapest.Type, e.Cheapest.Zone)
}
