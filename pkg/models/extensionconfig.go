package models

// HostTriggers defines CSS selectors for a specific hostname.
// A state is triggered when ALL selectors in the corresponding list match at least one element.
type HostTriggers struct {
	Loaded []string `yaml:"loaded" json:"loaded"` // All selectors must match for a successful load
	Failed []string `yaml:"failed" json:"failed"` // All selectors must match to indicate a failure
}
