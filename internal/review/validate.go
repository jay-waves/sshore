package review

import (
	"fmt"
	"github.com/jay-waves/sshore/internal/files"
	"github.com/jay-waves/sshore/internal/forward"
	"strings"
)

func ValidateOperations(ops []Operation) error {
	if len(ops) == 0 || len(ops) > MaxItems {
		return fmt.Errorf("request must contain 1–%d operations", MaxItems)
	}
	var total int64
	for _, op := range ops {
		if len(op.LocalHint) > 4096 || strings.ContainsAny(op.LocalHint, "\x00\r\n") {
			return fmt.Errorf("invalid local endpoint hint")
		}
		switch op.Kind {
		case "download", "upload":
			// The remote server is Linux, even when the client is Windows.
			if !strings.HasPrefix(op.Path, "/") || len(op.Path) > 4096 || strings.ContainsAny(op.Path, "\x00\r\n") {
				return fmt.Errorf("invalid remote path")
			}
			if op.Port != 0 || op.TargetPort != 0 || op.Reverse {
				return fmt.Errorf("unexpected port in file request")
			}
			if op.Kind == "download" {
				if err := files.ValidateName(op.Name); err != nil {
					return err
				}
				if op.Size < 0 || op.Size > (1<<63-1)-total {
					return fmt.Errorf("invalid download size")
				}
				total += op.Size
			} else if op.Name != "" || op.Size != 0 {
				return fmt.Errorf("unexpected upload metadata")
			}
		case "forward":
			if op.LocalHint != "" {
				if _, err := forward.ParsePort(op.LocalHint); err != nil {
					return err
				}
			}
			if len(ops) != 1 || op.Port < 0 || op.Port > 65535 || op.TargetPort < 0 || op.TargetPort > 65535 || op.ServerPort() == 0 || op.Path != "" || op.Name != "" || op.Size != 0 {
				return fmt.Errorf("invalid port request")
			}
		default:
			return fmt.Errorf("unsupported requested operation")
		}
	}
	return nil
}
