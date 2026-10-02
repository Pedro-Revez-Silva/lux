package server

import (
	"net/http"
	"strings"

	"github.com/marcioapm/lux/internal/spec"
)

// takesAttachments: the adapter has somewhere to put an image (every agent
// adapter; a generic workload's stdin is not one).
func takesAttachments(adapter string) bool {
	return adapter != "" && adapter != "generic"
}

func errAttachmentsUnsupported(adapter string) error {
	return errf(http.StatusBadRequest, "attachments_unsupported",
		"the Run's adapter is %s: a plain process has nowhere to put an image", adapter)
}

// checkAttachments is the attachments' metadata (nil for none), or a 400
// invalid_attachment naming every problem (by index).
func checkAttachments(at string, list []spec.Attachment) ([]spec.AttachmentMeta, error) {
	meta, problems := spec.CheckAttachments(at, list)
	if len(problems) == 0 {
		return meta, nil
	}
	he := errf(http.StatusBadRequest, "invalid_attachment", "%s", strings.Join(problems, "; "))
	he.Details = problems
	return nil, he
}

// splitPromptAttachments is the spec as stored, its attachments' names and
// types only, and their bytes (runs.prompt_attachments), nil for none.
func splitPromptAttachments(sp spec.RunSpec) (spec.RunSpec, []spec.Attachment) {
	full := sp.Workload.Attachments
	if len(full) == 0 {
		return sp, nil
	}
	list := make([]spec.Attachment, len(full))
	for i, a := range full {
		list[i] = spec.Attachment{Name: a.Name, ContentType: a.ContentType}
	}
	sp.Workload.Attachments = list
	return sp, full
}
