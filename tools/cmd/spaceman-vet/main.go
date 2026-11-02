// Command spaceman-vet runs SpaceMan's custom analyzers. Use with: go vet -vettool=$(which spaceman-vet) ./...
package main

import (
	"golang.org/x/tools/go/analysis/unitchecker"

	"github.com/aurimasniekis/go-spaceman/tools/analyzers/txscope"
)

func main() {
	unitchecker.Main(txscope.Analyzer)
}
