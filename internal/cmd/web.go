package cmd

import "github.com/spf13/cobra"

func init() {
	webCmd.Flags().StringP("host", "H", "localhost", "Host to listen on")
	webCmd.Flags().Bool("allow-cleartext", false, "Required to bind a non-loopback host; the UI uses plain HTTP and its per-process token is the only protection")
	webCmd.Flags().IntP("port", "p", 0, "Port to listen on (0 = random free port)")
	webCmd.Flags().Bool("no-open", false, "Do not open browser automatically")
	rootCmd.AddCommand(webCmd)
}

// webCmd is an explicit alias for the bare ` + "`rush`" + ` command. Both start the
// embedded web server — webCmd just makes the intent obvious in scripts.
var webCmd = &cobra.Command{
	Use:   "web",
	Args:  cobra.NoArgs,
	Short: "Start the Rush web UI (same as running 'rush' with no subcommand)",
	Long: `Start the Rush web UI.

Launches an HTTP server with a React UI and a WebSocket endpoint for
real-time communication with the agent. Sessions, permissions, model
selection, logs, message queueing, and turn-interruption (the yellow
"Interrupt" button that cancels the running turn while keeping
everything produced so far) all happen in the browser.

Remote browsers cannot connect to a non-loopback listener: WebSocket origins
are restricted to loopback. For remote access, use SSH port forwarding with
--host localhost.

At startup the server prints the URL and a one-time access token. The
token is also copied to your clipboard. Paste it in the browser to
authenticate — it is never transmitted as part of the URL.

  rush web
  rush web --port 8080 --no-open
  rush web --host 0.0.0.0 --allow-cleartext --port 9000   # bind on all interfaces; remote browsers cannot connect
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWebMode(cmd)
	},
}
