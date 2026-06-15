package web

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	fwresults "github.com/ClaudioSchirmer/omnicore/application/results"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	fwresponses "github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

// ─── Fixtures ──────────────────────────────────────────────────────────────

type specInsertCmd struct {
	pipeline.CommandBase
	Name string
}

type specUpdateCmd struct {
	pipeline.CommandBaseWithID
	Name string
}

type specInsertReq struct {
	Name string `json:"name"`
}

func (r specInsertReq) ToCommand() *specInsertCmd { return &specInsertCmd{Name: r.Name} }

type specUpdateReq struct {
	Name string `json:"name"`
}

func (r specUpdateReq) ToCommand() *specUpdateCmd { return &specUpdateCmd{Name: r.Name} }

type specInsertResult struct{ ID string }

type specInsertResp struct {
	ID string `json:"id"`
}

func (specInsertResp) FromResult(r specInsertResult) specInsertResp { return specInsertResp{ID: r.ID} }

type specLenientInsertHandler struct{}

func (specLenientInsertHandler) Handle(_ *configuration.AppContext, _ *specInsertCmd) (specInsertResult, error) {
	return specInsertResult{}, nil
}

type specStrictUpdateHandler struct {
	pipeline.FullBody
}

func (specStrictUpdateHandler) Handle(_ *configuration.AppContext, _ *specUpdateCmd) (specInsertResult, error) {
	return specInsertResult{}, nil
}

type specBodylessIDHandler struct{}

func (specBodylessIDHandler) Handle(_ *configuration.AppContext, _ *specUpdateCmd) (fwresults.None, error) {
	return fwresults.None{}, nil
}

// ─── HandleCommandWithBodySpec ─────────────────────────────────────────────

func TestHandleCommandWithBodySpec_LenientHandler_StrictFalse(t *testing.T) {
	pipe := pipeline.New(translation.New())
	h, spec := HandleCommandWithBodySpec[
		specInsertReq, specInsertCmd, *specInsertCmd, specInsertResult, specInsertResp,
	](pipe, specInsertReq{}, specInsertResp{}.FromResult, specLenientInsertHandler{}, fiber.StatusCreated)

	if h == nil {
		t.Fatal("handler should not be nil")
	}
	if spec.Strict {
		t.Fatal("lenient handler should yield Strict=false")
	}
	if spec.HasPathID {
		t.Fatal("HandleCommandWithBody is bodyless on the path side; HasPathID must be false")
	}
	if spec.SuccessStatus != fiber.StatusCreated {
		t.Fatalf("SuccessStatus: got %d, want 201", spec.SuccessStatus)
	}
	if spec.RequestType == nil || spec.RequestType.Name() != "specInsertReq" {
		t.Fatalf("RequestType: got %+v, want specInsertReq", spec.RequestType)
	}
	if spec.ResponseType == nil || spec.ResponseType.Name() != "specInsertResp" {
		t.Fatalf("ResponseType: got %+v, want specInsertResp", spec.ResponseType)
	}
}

// ─── HandleCommandWithBodyIDSpec ───────────────────────────────────────────

func TestHandleCommandWithBodyIDSpec_StrictHandler_StrictTrue(t *testing.T) {
	pipe := pipeline.New(translation.New())
	h, spec := HandleCommandWithBodyIDSpec[
		specUpdateReq, specUpdateCmd, *specUpdateCmd, specInsertResult, specInsertResp,
	](pipe, specUpdateReq{}, specInsertResp{}.FromResult, specStrictUpdateHandler{}, fiber.StatusOK)

	if h == nil {
		t.Fatal("handler should not be nil")
	}
	if !spec.Strict {
		t.Fatal("handler embedding pipeline.FullBody should yield Strict=true")
	}
	if !spec.HasPathID {
		t.Fatal("HandleCommandWithBodyID auto-binds :id; HasPathID must be true")
	}
	if spec.SuccessStatus != fiber.StatusOK {
		t.Fatalf("SuccessStatus: got %d, want 200", spec.SuccessStatus)
	}
}

// ─── HandleCommandWithIDSpec ───────────────────────────────────────────────

func TestHandleCommandWithIDSpec_NoBodyNoneResponse(t *testing.T) {
	pipe := pipeline.New(translation.New())
	h, spec := HandleCommandWithIDSpec[
		specUpdateCmd, *specUpdateCmd, fwresults.None, fwresponses.None,
	](pipe, fwresponses.NoBody, specBodylessIDHandler{}, fiber.StatusOK)

	if h == nil {
		t.Fatal("handler should not be nil")
	}
	if spec.RequestType != nil {
		t.Fatalf("bodyless route must report RequestType=nil; got %+v", spec.RequestType)
	}
	if !spec.HasPathID {
		t.Fatal("HandleCommandWithID auto-binds :id; HasPathID must be true")
	}
	if spec.ResponseType == nil || spec.ResponseType.Name() != "None" {
		t.Fatalf("ResponseType: got %+v, want responses.None", spec.ResponseType)
	}
	if spec.Strict {
		t.Fatal("HandleCommandWithID does not consult FullBody; Strict must stay false")
	}
}
