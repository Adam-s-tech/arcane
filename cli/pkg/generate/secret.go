package generate

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

var (
	secretFormat string
	secretLength int
)

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Generate cryptographic secrets",
	Long:  `Generate a secure cryptographic secret for ENCRYPTION_KEY.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return generateSecretsInternal(cmd.OutOrStdout())
	},
}

func init() {
	GenerateCmd.AddCommand(secretCmd)
	secretCmd.Flags().StringVarP(&secretFormat, "format", "f", "base64", "output format: base64, hex, env, docker, all")
	secretCmd.Flags().IntVarP(&secretLength, "length", "l", 32, "secret length in bytes (default: 32 for AES-256)")
}

func generateSecretsInternal(w io.Writer) error {
	encryptionKey := make([]byte, secretLength)
	if _, err := rand.Read(encryptionKey); err != nil {
		return fmt.Errorf("failed to generate encryption key: %w", err)
	}

	switch secretFormat {
	case "base64":
		printBase64FormatInternal(w, encryptionKey)
	case "hex":
		printHexFormatInternal(w, encryptionKey)
	case "env":
		printEnvFormatInternal(w, encryptionKey)
	case "docker":
		printDockerFormatInternal(w, encryptionKey)
	case "all":
		printAllFormatsInternal(w, encryptionKey)
	default:
		return fmt.Errorf("unknown format: %s (supported: base64, hex, env, docker, all)", secretFormat)
	}

	return nil
}

func printBase64FormatInternal(w io.Writer, encKey []byte) {
	_, _ = fmt.Fprintln(w, "BASE64")
	_, _ = fmt.Fprintln(w, "------")
	_, _ = fmt.Fprintf(w, "ENCRYPTION_KEY=%s\n", base64.StdEncoding.EncodeToString(encKey))
}

func printHexFormatInternal(w io.Writer, encKey []byte) {
	_, _ = fmt.Fprintln(w, "HEX")
	_, _ = fmt.Fprintln(w, "---")
	_, _ = fmt.Fprintf(w, "ENCRYPTION_KEY=%s\n", hex.EncodeToString(encKey))
}

func printEnvFormatInternal(w io.Writer, encKey []byte) {
	_, _ = fmt.Fprintln(w, "ENV (.env) FORMAT")
	_, _ = fmt.Fprintln(w, "-------------------")
	_, _ = fmt.Fprintf(w, "ENCRYPTION_KEY=%s\n", base64.StdEncoding.EncodeToString(encKey))
}

func printDockerFormatInternal(w io.Writer, encKey []byte) {
	_, _ = fmt.Fprintln(w, "DOCKER COMPOSE ENVIRONMENT")
	_, _ = fmt.Fprintln(w, "--------------------------")
	_, _ = fmt.Fprintln(w, "environment:")
	_, _ = fmt.Fprintf(w, "  - ENCRYPTION_KEY=%s\n", base64.StdEncoding.EncodeToString(encKey))
}

func printAllFormatsInternal(w io.Writer, encKey []byte) {
	_, _ = fmt.Fprintln(w, "Arcane cryptographic secrets")
	_, _ = fmt.Fprintln(w, "===========================")
	_, _ = fmt.Fprintln(w)

	_, _ = fmt.Fprintln(w, "ENV (.env) - recommended")
	_, _ = fmt.Fprintln(w, "------------------------")
	_, _ = fmt.Fprintf(w, "ENCRYPTION_KEY=%s\n", base64.StdEncoding.EncodeToString(encKey))
	_, _ = fmt.Fprintln(w)

	_, _ = fmt.Fprintln(w, "Docker Compose (environment block)")
	_, _ = fmt.Fprintln(w, "-------------------------------")
	_, _ = fmt.Fprintln(w, "environment:")
	_, _ = fmt.Fprintf(w, "  - ENCRYPTION_KEY=%s\n", base64.StdEncoding.EncodeToString(encKey))
	_, _ = fmt.Fprintln(w)

	_, _ = fmt.Fprintln(w, "HEX")
	_, _ = fmt.Fprintln(w, "---")
	_, _ = fmt.Fprintf(w, "ENCRYPTION_KEY=%s\n", hex.EncodeToString(encKey))
	_, _ = fmt.Fprintln(w)
}
