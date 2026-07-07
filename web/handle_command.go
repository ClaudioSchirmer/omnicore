package web

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/web/responses"
	"github.com/gofiber/fiber/v3"
)

// expectedKeysCache memoizes reflectExpectedJSONKeys by reflect.Type. Each
// wrapper already captures expectedKeys once at construction (closure), but
// the module-level cache avoids repeating the inspection when the same type
// appears in more than one route.
var expectedKeysCache sync.Map // map[reflect.Type][]string

// CommandByID creates a fiber.Handler for endpoints WITHOUT a body
// that receive the ID via the path (Archive, Unarchive, Delete). There is
// no BodyParser and no FullBody inspection — it only injects
// cmd.SetPathID(c.Params("id")) and dispatches.
//
// TResult is the application-layer Result produced by the handler's Project;
// TResp is the wire shape rendered by responseProjection (typically a method
// value like `MyResponse{}.FromResult`). When the consumer does not project
// anything back to the wire, pass `responses.NoBody` — the wrapper detects
// the responses.None type at runtime and emits the success envelope WITHOUT
// a "data" field.
//
// Endpoints with a body use CommandWithBody{,ID} instead of this
// wrapper.
//
//	app.Patch("/users/:id/archive", web.CommandByID(pipe,
//	    responses.NoBody,
//	    &handlers.ArchiveCommandHandler[*User, *ArchiveUserCommand, results.None]{
//	        Repo: r, Auditor: a, Service: svc,
//	        Project: results.NoProjection[*User],
//	    },
//	    fiber.StatusOK))
func CommandByID[
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandByID
	},
	TResult any,
	TResp any,
](
	pipe *pipeline.Pipeline,
	responseProjection func(TResult) TResp,
	h pipeline.Handler[TCmdPtr, TResult],
	successStatus int,
) fiber.Handler {
	return func(c fiber.Ctx) error {
		cmd := TCmdPtr(new(TCmd))
		cmd.SetPathID(c.Params("id"))
		appCtx := AppContext(c)
		appCtx.SetParentIfAbsent(c)
		result := pipeline.Dispatch(pipe, appCtx, cmd, h)
		return respondWithProjection(c, result, successStatus, responseProjection)
	}
}

// respondWithProjection is the shared success/failure router used by every
// CommandWith* wrapper. On Success, applies the consumer's response
// projection to the handler's TResult and emits the wire envelope; if the
// resulting wire shape is responses.None (the framework's empty default),
// the envelope is emitted without a "data" field. On Failure/Exception,
// delegates to RespondFromResult which honors each notification's Semantic.
func respondWithProjection[TResult any, TResp any](
	c fiber.Ctx,
	result pipeline.Result[TResult],
	successStatus int,
	project func(TResult) TResp,
) error {
	if result.IsSuccess() {
		wire := project(result.Value())
		if _, isNone := any(wire).(responses.None); isNone {
			return RespondWithStatus(c, successStatus)
		}
		return RespondWithSuccess(c, successStatus, wire)
	}
	return RespondFromResult(c, result, successStatus)
}

// inspectHandler returns (strict, expectedKeys) by consulting the
// FullBodyEnforcer marker on the handler. When absent → strict=false and
// expectedKeys nil (lenient path). Used by CommandWithBody{,ID} — the
// reflection runs on the Request type (TReq), not on the Command.
func inspectHandler[T any](h any) (bool, []string) {
	if _, ok := h.(pipeline.FullBodyEnforcer); !ok {
		return false, nil
	}
	t := reflect.TypeOf((*T)(nil)).Elem()
	return true, reflectExpectedJSONKeys(t)
}

// reflectExpectedJSONKeys lists the JSON keys expected in the body of T:
// non-anonymous exported fields, with a json tag that is not "-". Keys derive
// from the first segment of the json tag (before the comma) or from the
// field name when the tag is absent. Fields carrying a `path:"<name>"` tag
// are skipped — their value comes from the URL segment, not the body, so
// the strict-body check would otherwise mark them as wrongly missing (see
// the path-tag design §3.5). The result is sorted for diff/test
// determinism and cached by reflect.Type.
func reflectExpectedJSONKeys(t reflect.Type) []string {
	if cached, ok := expectedKeysCache.Load(t); ok {
		return cached.([]string)
	}
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			continue
		}
		if !f.IsExported() {
			continue
		}
		if f.Tag.Get("path") != "" {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name := f.Name
		if tag != "" {
			if comma := strings.IndexByte(tag, ','); comma >= 0 {
				tag = tag[:comma]
			}
			if tag != "" {
				name = tag
			}
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	expectedKeysCache.Store(t, keys)
	return keys
}

// missingKeys returns the elements of expected absent from raw, preserving
// the order of expected (already sorted by reflectExpectedJSONKeys).
func missingKeys(expected []string, raw map[string]json.RawMessage) []string {
	missing := make([]string, 0)
	for _, k := range expected {
		if _, ok := raw[k]; !ok {
			missing = append(missing, k)
		}
	}
	return missing
}

// respondMissingFields is the legacy helper that emits
// RequiredFieldNotification with default semantic (Validation → 422). Kept
// for compat with existing tests that were not migrated;
// CommandWithBody{,ID} uses respondMissingFieldsAsSchema (in
// handle_command_with_body.go) which triggers 400.
//
// Deprecated: for new uses, prefer respondMissingFieldsAsSchema.
func respondMissingFields[TRes any](c fiber.Ctx, pipe *pipeline.Pipeline, missing []string) error {
	ctx := domain.NewNotificationContext("Request")
	for _, field := range missing {
		ctx.AddNotificationMessage(domain.NotificationMessage{
			FieldName:    field,
			Notification: domain.RequiredFieldNotification{},
		})
	}
	err := domain.NewDomainError([]*domain.NotificationContext{ctx})
	result := pipeline.Run(pipe, AppContext(c), func() (TRes, error) {
		var zero TRes
		return zero, err
	})
	return RespondFromResult(c, result, fiber.StatusOK)
}
