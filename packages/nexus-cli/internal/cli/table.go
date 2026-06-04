package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
)

// table prints an aligned text table to a.Out: a header row followed by one line
// per data row, using the CLI's standard tabwriter padding (the settings every
// list command shared by copy-paste). It is the single hand-built table renderer
// for the typed list commands; generic JSON collections render through restable.
// Cells are pre-formatted strings, so each caller formats its own numbers/flags.
func (a *App) table(header []string, rows [][]string) error {
	tw := tabwriter.NewWriter(a.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}
