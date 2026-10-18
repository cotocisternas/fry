module github.com/emersion/go-imap/v2

go 1.26.0

require (
	github.com/emersion/go-message v0.18.2
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6
)

// Standalone fork graph hardening; root replacement directives are not inherited.
// These unused go-message/tools dependencies otherwise retain vulnerable releases after tidy.
replace golang.org/x/text => golang.org/x/text v0.43.0

replace golang.org/x/net => golang.org/x/net v0.61.0

replace github.com/yuin/goldmark v1.4.13 => github.com/yuin/goldmark v1.8.6
