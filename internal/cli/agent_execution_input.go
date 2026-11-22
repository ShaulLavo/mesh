package cli

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/spf13/cobra"
)

const agentSessionInput = "sessions.input"

type executionInputOptions struct {
	executionControlOptions
	writeKey, offset, text, base64, file string
	eof                                  bool
}

func (a *application) sessionInputCommand() *cobra.Command {
	o := &executionInputOptions{}
	cmd := &cobra.Command{
		Use:   "input SESSION_ID",
		Short: "Write pipe stdin once or look up a write receipt",
		Args:  exactExecutionArgs(agentSessionInput),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.validateControl(agentSessionInput, args[0], protocol.TypeExecutionInput); err != nil {
				return err
			}
			req, err := o.request(cmd, args[0])
			if err != nil {
				return agentapi.Usage(agentSessionInput, err.Error())
			}
			return a.runAgentExecution(cmd, o.agentExecutionOptions, req)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&o.host, "host", "", "online machine name or exact host ID, required")
	flags.BoolVar(&o.json, "json", false, "write one agent result, required")
	flags.DurationVar(&o.timeout, "timeout", 5*time.Second, "finite deadline from 100ms to 60s")
	flags.StringVar(&o.instance, "instance", "", "exact instance from the creation receipt, required")
	flags.StringVar(&o.writeKey, "write-key", "", "saved key for one logical write, required")
	flags.StringVar(&o.offset, "offset", "", "expected byte offset, omitted to look up the write key")
	flags.StringVar(&o.text, "text", "", "write exact text bytes")
	flags.StringVar(&o.base64, "base64", "", "write Base64-decoded bytes")
	flags.StringVar(&o.file, "file", "", "write up to 1 MiB from a local file, or - for stdin")
	flags.BoolVar(&o.eof, "eof", false, "close pipe stdin after these bytes")
	cmd.MarkFlagsMutuallyExclusive("text", "base64", "file")
	return cmd
}

func (o executionInputOptions) request(cmd *cobra.Command, id string) (protocol.Control, error) {
	req := protocol.Control{Type: protocol.TypeExecutionInput, SessionID: id, Instance: o.instance, WriteKey: o.writeKey, InputEOF: o.eof}
	if err := session.ValidateWriteKey(o.writeKey); err != nil {
		return req, fmt.Errorf("input: %w", err)
	}
	if !cmd.Flags().Changed("offset") {
		if o.eof || cmd.Flags().Changed("text") || cmd.Flags().Changed("base64") || cmd.Flags().Changed("file") {
			return req, fmt.Errorf("pass --offset when writing bytes or EOF")
		}
		return req, nil
	}
	offset, err := strconv.ParseUint(o.offset, 10, 64)
	if err != nil || strconv.FormatUint(offset, 10) != o.offset {
		return req, fmt.Errorf("pass a nonnegative decimal --offset")
	}
	req.InputOffset = &offset
	req.InputBytes, err = o.bytes(cmd)
	if err != nil {
		return req, err
	}
	_, err = req.InputIntent()
	return req, executionBoundaryError("validate stdin request", err)
}

func (o executionInputOptions) bytes(cmd *cobra.Command) ([]byte, error) {
	switch {
	case cmd.Flags().Changed("file"):
		return readExecutionInputFile(cmd.InOrStdin(), o.file)
	case cmd.Flags().Changed("base64"):
		data, err := base64.StdEncoding.Strict().DecodeString(o.base64)
		return data, executionBoundaryError("decode --base64", err)
	default:
		return []byte(o.text), nil
	}
}

func readExecutionInputFile(input io.Reader, name string) ([]byte, error) {
	if name != "-" {
		file, err := os.Open(name) //nolint:gosec // explicitly chosen CLI input file
		if err != nil {
			return nil, fmt.Errorf("open input file: %w", err)
		}
		defer func() { _ = file.Close() }()
		input = file
	}
	data, err := io.ReadAll(io.LimitReader(input, session.MaxInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read input: %w", err)
	}
	return data, nil
}

func agentExecutionInputData(response, request protocol.Control) (any, error) {
	receipt := response.InputReceipt
	if receipt == nil {
		return nil, nil
	}
	intent, err := request.InputIntent()
	if err != nil || receipt.WriteKey != request.WriteKey || receipt.Length < 0 || receipt.Length > session.MaxInputBytes ||
		(intent != nil && *intent != *receipt) || response.ExecutionReceipt == nil ||
		response.ExecutionReceipt.SessionID != request.SessionID || response.ExecutionReceipt.Instance != request.Instance {
		return nil, fmt.Errorf("host returned an invalid stdin receipt")
	}
	return receipt, nil
}
