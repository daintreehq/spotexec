package provider

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SpotPrice is the current Spot price of one instance type in one
// availability zone.
type SpotPrice struct {
	Region string    `json:"region"`
	Zone   string    `json:"zone"`
	Type   string    `json:"type"`
	Price  string    `json:"price"` // USD per hour, exactly as AWS reports it
	Since  time.Time `json:"since"` // when this price took effect
}

// USD is the hourly price as a number (0 when malformed, which parsing
// already rejects).
func (p SpotPrice) USD() float64 {
	f, _ := strconv.ParseFloat(p.Price, 64)
	return f
}

// Linux product descriptions with identical pricing. Licensed variants
// (SUSE, Red Hat, Windows) are never mixed in.
var linuxProducts = []string{"Linux/UNIX", "Linux/UNIX (Amazon VPC)"}

func isLinuxProduct(s string) bool {
	for _, p := range linuxProducts {
		if s == p {
			return true
		}
	}
	return false
}

// parseSpotPriceHistory reads one DescribeSpotPriceHistory page.
func parseSpotPriceHistory(region string, b []byte) ([]SpotPrice, string, error) {
	var v struct {
		Items []struct {
			Type    string `xml:"instanceType"`
			Product string `xml:"productDescription"`
			Price   string `xml:"spotPrice"`
			Time    string `xml:"timestamp"`
			Zone    string `xml:"availabilityZone"`
		} `xml:"spotPriceHistorySet>item"`
		NextToken string `xml:"nextToken"`
	}
	if err := xml.Unmarshal(b, &v); err != nil {
		return nil, "", fmt.Errorf("parse DescribeSpotPriceHistory: %w", err)
	}
	var out []SpotPrice
	for _, it := range v.Items {
		if !isLinuxProduct(it.Product) || it.Zone == "" || it.Type == "" {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(it.Price), 64)
		if err != nil || !(f > 0) || f > 1e6 {
			continue // never treat a missing or malformed price as cheap
		}
		t, err := time.Parse(time.RFC3339, it.Time)
		if err != nil {
			continue
		}
		out = append(out, SpotPrice{Region: region, Zone: it.Zone, Type: it.Type, Price: strings.TrimSpace(it.Price), Since: t})
	}
	return out, strings.TrimSpace(v.NextToken), nil
}

// latestPrices keeps, for each zone and type, the price that took effect
// last at or before now: the one in force. (Not the lowest the history
// shows, which may have stopped applying long ago.)
func latestPrices(rows []SpotPrice, now time.Time) []SpotPrice {
	best := map[string]SpotPrice{}
	for _, r := range rows {
		if r.Since.After(now) {
			continue
		}
		k := r.Region + "/" + r.Zone + "/" + r.Type
		if cur, ok := best[k]; !ok || r.Since.After(cur.Since) {
			best[k] = r
		}
	}
	out := make([]SpotPrice, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	rankPrices(out)
	return out
}

// rankPrices orders cheapest first; ties break by region, zone and type
// so every process ranks the same way.
func rankPrices(ps []SpotPrice) {
	sort.SliceStable(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		if fa, fb := a.USD(), b.USD(); fa != fb {
			return fa < fb
		}
		if a.Region != b.Region {
			return a.Region < b.Region
		}
		if a.Zone != b.Zone {
			return a.Zone < b.Zone
		}
		return a.Type < b.Type
	})
}

// spotPrices fetches the current Linux Spot price of each instance type in
// every availability zone of a's region. A type the region doesn't offer
// simply has no rows.
func (a *AWSSpot) spotPrices(ctx context.Context, types []string, now time.Time) ([]SpotPrice, error) {
	var rows []SpotPrice
	seen := map[string]bool{}
	token := ""
	for page := 0; ; page++ {
		p := url.Values{}
		for i, t := range types {
			p.Set("InstanceType."+strconv.Itoa(i+1), t)
		}
		for i, d := range linuxProducts {
			p.Set("ProductDescription."+strconv.Itoa(i+1), d)
		}
		// From now: AWS returns the price in force at StartTime for every
		// zone and type, plus any later changes.
		p.Set("StartTime", now.UTC().Format(time.RFC3339))
		p.Set("MaxResults", "1000")
		if token != "" {
			p.Set("NextToken", token)
		}
		b, err := a.api.ec2(ctx, "DescribeSpotPriceHistory", p)
		if err != nil {
			return nil, err
		}
		got, next, err := parseSpotPriceHistory(a.r.Region, b)
		if err != nil {
			return nil, err
		}
		rows = append(rows, got...)
		if next == "" {
			break
		}
		if seen[next] || page > 50 {
			return nil, errors.New("DescribeSpotPriceHistory pagination did not end")
		}
		seen[next] = true
		token = next
	}
	return latestPrices(rows, now.Add(time.Minute)), nil
}

// enabledRegions lists the regions enabled for this account (opted-in or
// not needing opt-in), as seen from a's region.
func (a *AWSSpot) enabledRegions(ctx context.Context) ([]string, error) {
	b, err := a.api.ec2(ctx, "DescribeRegions", nil)
	if err != nil {
		return nil, err
	}
	return parseDescribeRegions(b)
}

func parseDescribeRegions(b []byte) ([]string, error) {
	var v struct {
		Items []struct {
			Name   string `xml:"regionName"`
			OptIn  string `xml:"optInStatus"`
			Points string `xml:"regionEndpoint"`
		} `xml:"regionInfo>item"`
	}
	if err := xml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("parse DescribeRegions: %w", err)
	}
	var out []string
	for _, it := range v.Items {
		if it.OptIn == "not-opted-in" || it.Name == "" {
			continue
		}
		out = append(out, it.Name)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, errors.New("DescribeRegions returned no enabled regions")
	}
	return out, nil
}
