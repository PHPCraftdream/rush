package cmd

import "strings"

// The oracle is deliberately a literal copy, NOT derived from production
// code: widening an allowlist must take a matching edit here.
var journalOracleFlags = map[string]bool{
	"session": true, "role": true, "model": true, "effort": true,
	"timeout": true, "cwd": true, "data-dir": true, "n": true,
	"audit-days": true, "color-scheme": true, "host": true, "port": true,
	"agents": true, "max-cost": true, "max-tokens": true,
	"wait": true, "by": true, "at": true,
}

var journalOraclePositional = map[string]bool{
	"rush sessions show": true, "rush sessions last": true,
	"rush sessions tail": true, "rush sessions watch": true,
	"rush sessions kill": true, "rush sessions why": true,
	"rush sessions locks": true, "rush sessions fork": true,
	"rush sessions cancel": true, "rush sessions tree": true,
	"rush sessions cost": true, "rush sessions diff": true,
	"rush sessions reset": true, "rush sessions inject": true,
	"rush models use": true, "rush models bump": true, "rush models unset": true,
	"rush providers show": true, "rush providers set": true,
	"rush providers add": true, "rush providers update": true,
	"rush providers enable": true, "rush providers disable": true,
	"rush providers unset": true, "rush providers fetch-models": true,
	"rush providers test": true, "rush providers patch": true,
	"rush mcp show": true, "rush mcp enable": true, "rush mcp disable": true,
	"rush mcp restart": true, "rush mcp test": true, "rush mcp add": true,
	"rush mcp remove": true, "rush mcp set": true,
}

// journalOracleFlagAllowed is the independent expectation for string flags.
func journalOracleFlagAllowed(name, path string) bool {
	if name == "format" {
		return strings.HasPrefix(path, "rush sessions ")
	}
	return journalOracleFlags[name]
}
