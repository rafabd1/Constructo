package search

import "context"

// Searcher defines the interface for performing searches.
type Searcher interface {
	Search(ctx context.Context, query string) ([]Result, error)
}

// Result represents a single search result.
type Result struct {
	Title   string
	URL     string
	Snippet string
} 