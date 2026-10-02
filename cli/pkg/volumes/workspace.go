package volumes

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/getarcaneapp/arcane/types/v2/base"
	"github.com/getarcaneapp/arcane/types/v2/volume"
	workspacetypes "github.com/getarcaneapp/arcane/types/v2/workspace"
	"github.com/spf13/cobra"

	"github.com/getarcaneapp/arcane/cli/v2/internal/cmdutil"
	"github.com/getarcaneapp/arcane/cli/v2/internal/output"
	"github.com/getarcaneapp/arcane/cli/v2/internal/prompt"
	"github.com/getarcaneapp/arcane/cli/v2/internal/types"
)

var workspaceCmd = &cobra.Command{
	Use:          "workspace <volume-name> [path]",
	Short:        "Browse volume workspace files",
	Args:         cobra.RangeArgs(1, 2),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := cmdutil.ClientFromCommand(cmd)
		if err != nil {
			return err
		}

		allowPrompt := !jsonOutput && prompt.IsInteractive()
		resolved, _, err := volumeRef.Resolve(cmd.Context(), c, args[0], allowPrompt)
		if err != nil {
			return err
		}

		resp, err := c.Get(cmd.Context(), types.VolumeWorkspace(c.EnvID(), resolved.Name))
		if err != nil {
			return fmt.Errorf("failed to get volume workspace: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if ensureSuccessStatusErr := cmdutil.EnsureSuccessStatus(resp); ensureSuccessStatusErr != nil {
			return fmt.Errorf("failed to get volume workspace: %w", ensureSuccessStatusErr)
		}

		var result base.ApiResponse[workspacetypes.Workspace]
		if decodeJSONErr := cmdutil.DecodeJSON(resp, &result); decodeJSONErr != nil {
			return decodeJSONErr
		}

		// The endpoint returns the full tree; an optional path argument
		// narrows the listing to that subtree.
		entries := result.Data.Files
		if len(args) == 2 {
			prefix := strings.Trim(args[1], "/")
			filtered := entries[:0:0]
			for _, entry := range entries {
				if entry.RelativePath == prefix || strings.HasPrefix(entry.RelativePath, prefix+"/") {
					filtered = append(filtered, entry)
				}
			}
			entries = filtered
		}

		if jsonOutput {
			return cmdutil.PrintJSON(entries)
		}

		headers := []string{"PATH", "TYPE", "SIZE", "MODIFIED"}
		rows := make([][]string, len(entries))
		for i, entry := range entries {
			entryType := "file"
			switch {
			case entry.IsDirectory:
				entryType = "dir"
			case entry.IsSymlink:
				entryType = "symlink"
			}
			size := ""
			if !entry.IsDirectory {
				size = output.Bytes(entry.Size)
			}
			rows[i] = []string{
				entry.RelativePath,
				entryType,
				size,
				entry.ModTime.Format(time.RFC3339),
			}
		}
		if tableErr := output.Table(headers, rows); tableErr != nil {
			return tableErr
		}
		if result.Data.FileTreeTruncated {
			output.Warning("File listing was truncated by the server")
		}
		return nil
	},
}

var workspaceCatCmd = &cobra.Command{
	Use:          "cat <volume-name> <path>",
	Short:        "Print a volume workspace file",
	Args:         cobra.ExactArgs(2),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := cmdutil.ClientFromCommand(cmd)
		if err != nil {
			return err
		}

		allowPrompt := !jsonOutput && prompt.IsInteractive()
		resolved, _, err := volumeRef.Resolve(cmd.Context(), c, args[0], allowPrompt)
		if err != nil {
			return err
		}

		filePath := cmdutil.AppendQuery(
			types.VolumeWorkspaceFile(c.EnvID(), resolved.Name),
			url.Values{"relativePath": []string{args[1]}},
		)
		resp, err := c.Get(cmd.Context(), filePath)
		if err != nil {
			return fmt.Errorf("failed to get workspace file: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if ensureSuccessStatusErr := cmdutil.EnsureSuccessStatus(resp); ensureSuccessStatusErr != nil {
			return fmt.Errorf("failed to get workspace file: %w", ensureSuccessStatusErr)
		}

		var result base.ApiResponse[workspacetypes.FileContent]
		if decodeJSONErr := cmdutil.DecodeJSON(resp, &result); decodeJSONErr != nil {
			return decodeJSONErr
		}

		if jsonOutput {
			return cmdutil.PrintJSON(result.Data)
		}

		if result.Data.Content == "" && result.Data.ReadOnlyReason != "" {
			return fmt.Errorf("file %s has no printable content (%s); use `arcane volumes workspace download` instead", args[1], result.Data.ReadOnlyReason)
		}

		fmt.Print(result.Data.Content)
		return nil
	},
}

var workspaceDownloadCmd = &cobra.Command{
	Use:          "download <volume-name> <path> [output-file]",
	Short:        "Download a volume workspace file",
	Args:         cobra.RangeArgs(2, 3),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := cmdutil.ClientFromCommand(cmd)
		if err != nil {
			return err
		}

		allowPrompt := prompt.IsInteractive()
		resolved, _, err := volumeRef.Resolve(cmd.Context(), c, args[0], allowPrompt)
		if err != nil {
			return err
		}

		// Downloading large files can take a long time
		c.SetTimeout(30 * time.Minute)

		downloadPath := cmdutil.AppendQuery(
			types.VolumeWorkspaceFileDownload(c.EnvID(), resolved.Name),
			url.Values{"relativePath": []string{args[1]}},
		)
		resp, err := c.Get(cmd.Context(), downloadPath)
		if err != nil {
			return fmt.Errorf("failed to download workspace file: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if ensureSuccessStatusErr := cmdutil.EnsureSuccessStatus(resp); ensureSuccessStatusErr != nil {
			return fmt.Errorf("failed to download workspace file: %w", ensureSuccessStatusErr)
		}

		outputFile := ""
		if len(args) == 3 {
			outputFile = args[2]
		}
		if outputFile == "" {
			outputFile = downloadFilename(resp, path.Base(args[1]))
		}

		if writeResponseToFileErr := writeResponseToFile(resp.Body, outputFile); writeResponseToFileErr != nil {
			return writeResponseToFileErr
		}

		output.Success("File downloaded to %s", outputFile)
		return nil
	},
}

var workspacePutCmd = &cobra.Command{
	Use:          "put <volume-name> <local-file> <path>",
	Short:        "Write a local file into a volume workspace",
	Args:         cobra.ExactArgs(3),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := cmdutil.ClientFromCommand(cmd)
		if err != nil {
			return err
		}

		allowPrompt := !forceFlag && prompt.IsInteractive()
		resolved, _, err := volumeRef.Resolve(cmd.Context(), c, args[0], allowPrompt)
		if err != nil {
			return err
		}

		content, err := os.ReadFile(args[1])
		if err != nil {
			return fmt.Errorf("failed to read local file: %w", err)
		}

		relativePath := strings.Trim(filepath.ToSlash(args[2]), "/")
		if relativePath == "" {
			return errors.New("destination path is required")
		}

		if !forceFlag {
			confirmed, confirmErr := cmdutil.Confirm(cmd, fmt.Sprintf("Write %s to %s in volume %s?", args[1], relativePath, resolved.Name))
			if confirmErr != nil {
				return confirmErr
			}
			if !confirmed {
				fmt.Println("Cancelled")
				return nil
			}
		}

		workspaceResp, err := c.Get(cmd.Context(), types.VolumeWorkspace(c.EnvID(), resolved.Name))
		if err != nil {
			return fmt.Errorf("failed to get volume workspace: %w", err)
		}
		defer func() { _ = workspaceResp.Body.Close() }()
		if ensureSuccessStatusErr := cmdutil.EnsureSuccessStatus(workspaceResp); ensureSuccessStatusErr != nil {
			return fmt.Errorf("failed to get volume workspace: %w", ensureSuccessStatusErr)
		}
		var current base.ApiResponse[workspacetypes.Workspace]
		if decodeJSONErr := cmdutil.DecodeJSON(workspaceResp, &current); decodeJSONErr != nil {
			return decodeJSONErr
		}

		operation := volume.FileOpCreateFile
		for _, entry := range current.Data.Files {
			if entry.RelativePath == relativePath && !entry.IsDirectory {
				operation = volume.FileOpUpdateFile
				break
			}
		}

		uploadIndex := 0
		manifest := volume.WorkspaceUpdateManifest{
			FileTreeRevision: current.Data.FileTreeRevision,
			FileChanges: []volume.WorkspaceFileChange{{
				Operation:    operation,
				RelativePath: relativePath,
				UploadIndex:  &uploadIndex,
			}},
		}
		manifestJSON, err := json.Marshal(manifest)
		if err != nil {
			return fmt.Errorf("failed to encode workspace manifest: %w", err)
		}

		var requestBody bytes.Buffer
		writer := multipart.NewWriter(&requestBody)
		if writeFieldErr := writer.WriteField("manifest", string(manifestJSON)); writeFieldErr != nil {
			return fmt.Errorf("failed to write workspace manifest: %w", writeFieldErr)
		}
		filePart, err := writer.CreateFormFile("files", path.Base(relativePath))
		if err != nil {
			return fmt.Errorf("failed to create workspace upload: %w", err)
		}
		if _, writeErr := filePart.Write(content); writeErr != nil {
			return fmt.Errorf("failed to write workspace upload: %w", writeErr)
		}
		if closeErr := writer.Close(); closeErr != nil {
			return fmt.Errorf("failed to finalize workspace upload: %w", closeErr)
		}

		resp, err := c.RequestRaw(cmd.Context(), http.MethodPut, types.VolumeWorkspace(c.EnvID(), resolved.Name), &requestBody, map[string]string{"Content-Type": writer.FormDataContentType()})
		if err != nil {
			return fmt.Errorf("failed to update volume workspace: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if updateStatusErr := cmdutil.EnsureSuccessStatus(resp); updateStatusErr != nil {
			return fmt.Errorf("failed to update volume workspace: %w", updateStatusErr)
		}

		if jsonOutput {
			var result base.ApiResponse[workspacetypes.Workspace]
			if decodeUpdateErr := cmdutil.DecodeJSON(resp, &result); decodeUpdateErr != nil {
				return decodeUpdateErr
			}
			return cmdutil.PrintJSON(result.Data)
		}

		output.Success("File %s written to volume %s", relativePath, resolved.Name)
		return nil
	},
}

func init() {
	VolumesCmd.AddCommand(workspaceCmd)
	workspaceCmd.AddCommand(workspaceCatCmd)
	workspaceCmd.AddCommand(workspaceDownloadCmd)
	workspaceCmd.AddCommand(workspacePutCmd)

	workspaceCmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	workspaceCatCmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	workspacePutCmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Write without confirmation")
	workspacePutCmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
}
