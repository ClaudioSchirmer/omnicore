package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/gofiber/fiber/v3"
)

// CommandWithBody creates a fiber.Handler for endpoints with a JSON
// body without a path ID (POST). It separates wire format (Request DTO in
// web/) from use-case vocabulary (Command in application/) — orthodox DDD.
//
// Flow:
//  1. Allocates a zero-value TReq
//  2. Strict (handler embeds pipeline.FullBody): checks presence of ALL
//     Request fields in the body JSON. Missing body, missing field or
//     malformed JSON → 400 (not 422) with Schema Semantic.
//  3. BodyParser fills the Request. Type mismatch → 400 with
//     SchemaViolationNotification carrying the error's field.
//  4. req.ToCommand() produces the Command (web→application boundary).
//  5. Dispatch via Pipeline (translation + AppContext + defer/recover).
//  6. respondWithProjection applies responseProjection to the handler's
//     TResult — on Success, renders the wire envelope (or 200/204-no-data
//     when the projection lands on responses.None); on Failure/Exception,
//     RespondFromResult honors each notification's Semantic.
//
// The BodyParser runs on the Request, not on the Command — reflection for
// missing-field detection also runs on the Request. Domain remains unaware
// of JSON.
//
// HTTP semantics:
//
//   - 400 → wire contract violated (Request schema does not match)
//
//   - 422 → domain rejects values (notifications via BuildRules)
//
//   - other 4xx → custom semantics via Notification.Semantic()
//
//     app.Post("/users", web.CommandWithBody(pipe,
//     requests.InsertUserRequest{},
//     responses.InsertUserResponse{}.FromResult,
//     &handlers.InsertCommandHandler[*User, *InsertUserCommand, results.InsertUserResult]{
//     Repo: r, Auditor: a, Service: svc,
//     Project: results.InsertUserResult{}.FromEntity,
//     },
//     fiber.StatusCreated))
func CommandWithBody[
	TReq RequestDTO[TCmdPtr],
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandWithBody
	},
	TResult any,
	TResp any,
](
	pipe *pipeline.Pipeline,
	sample TReq,
	responseProjection func(TResult) TResp,
	h pipeline.Handler[TCmdPtr, TResult],
	successStatus int,
) fiber.Handler {
	_ = sample // anchors TReq for type inference; not used at runtime
	reqType := reflect.TypeOf((*TReq)(nil)).Elem()
	pathSchema := inspectPathTags(reqType)
	strict, expectedKeys := inspectHandler[TReq](h)
	warnGroupAMissingPathTag(h, "CommandWithBody", reqType, pathSchema)
	return func(c fiber.Ctx) error {
		body := c.Body()

		if strict && len(expectedKeys) > 0 {
			if len(body) == 0 {
				return respondMissingFieldsAsSchema[TResult](c, pipe, expectedKeys)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(body, &raw); err != nil {
				return respondSchemaViolation[TResult](c, pipe, "")
			}
			if missing := missingKeys(expectedKeys, raw); len(missing) > 0 {
				return respondMissingFieldsAsSchema[TResult](c, pipe, missing)
			}
		}

		var req TReq
		if badField, ok := applyPathBinding(c, pathSchema, reflect.ValueOf(&req)); !ok {
			return respondSchemaViolation[TResult](c, pipe, badField)
		}
		if len(body) > 0 {
			if err := c.Bind().Body(&req); err != nil {
				return respondSchemaViolation[TResult](c, pipe, extractBodyParserField(err))
			}
		}
		appCtx := AppContext(c)
		appCtx.SetParentIfAbsent(c)
		cmd := req.ToCommand()
		result := pipeline.Dispatch(pipe, appCtx, cmd, h)
		return respondWithProjection(c, result, successStatus, responseProjection)
	}
}

// CommandWithBodyID is a variant of CommandWithBody for Commands
// with an ID via the path (PUT/PATCH). Injects cmd.SetPathID(c.Params("id"))
// after ToCommand.
//
//	app.Put("/users/:id", web.CommandWithBodyID(pipe,
//	    requests.UpdateUserRequest{},
//	    responses.UpdateUserResponse{}.FromResult,
//	    &handlers.UpdateCommandHandler[*User, *UpdateUserCommand, results.UpdateUserResult]{
//	        Repo: r, Auditor: a, Service: svc,
//	        Project: results.UpdateUserResult{}.FromEntity,
//	    },
//	    fiber.StatusOK))
func CommandWithBodyID[
	TReq RequestDTO[TCmdPtr],
	TCmd any,
	TCmdPtr interface {
		*TCmd
		pipeline.CommandWithBodyID
	},
	TResult any,
	TResp any,
](
	pipe *pipeline.Pipeline,
	sample TReq,
	responseProjection func(TResult) TResp,
	h pipeline.Handler[TCmdPtr, TResult],
	successStatus int,
) fiber.Handler {
	_ = sample
	reqType := reflect.TypeOf((*TReq)(nil)).Elem()
	pathSchema := inspectPathTags(reqType)
	if hasPathSegment(reqType, "id") {
		panic(formatPathIDConflict("CommandWithBodyID", reqType))
	}
	strict, expectedKeys := inspectHandler[TReq](h)
	return func(c fiber.Ctx) error {
		body := c.Body()

		if strict && len(expectedKeys) > 0 {
			if len(body) == 0 {
				return respondMissingFieldsAsSchema[TResult](c, pipe, expectedKeys)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(body, &raw); err != nil {
				return respondSchemaViolation[TResult](c, pipe, "")
			}
			if missing := missingKeys(expectedKeys, raw); len(missing) > 0 {
				return respondMissingFieldsAsSchema[TResult](c, pipe, missing)
			}
		}

		var req TReq
		if badField, ok := applyPathBinding(c, pathSchema, reflect.ValueOf(&req)); !ok {
			return respondSchemaViolation[TResult](c, pipe, badField)
		}
		if len(body) > 0 {
			if err := c.Bind().Body(&req); err != nil {
				return respondSchemaViolation[TResult](c, pipe, extractBodyParserField(err))
			}
		}
		appCtx := AppContext(c)
		appCtx.SetParentIfAbsent(c)
		cmd := req.ToCommand()
		cmd.SetPathID(c.Params("id"))
		result := pipeline.Dispatch(pipe, appCtx, cmd, h)
		return respondWithProjection(c, result, successStatus, responseProjection)
	}
}

// formatPathIDConflict renders the boot diagnostic for the §4.1 conflict —
// declaring `path:"id"` on a Request used with a :id-binding wrapper.
func formatPathIDConflict(wrapperName string, reqType reflect.Type) string {
	return fmt.Sprintf(
		"\n[omnicore] FATAL: %s does not accept a `path:\"id\"` tag on the Request.\n\n"+
			"  wrapper:  %s\n  request:  %s\n\n"+
			"  This wrapper already binds the :id URL segment automatically through the\n"+
			"  pipeline.CommandByID / queries.QueryByID interface — declaring `path:\"id\"`\n"+
			"  on the Request would set the same value twice.\n\n"+
			"  Fix one of:\n"+
			"    (1) Remove the `path:\"id\"` tag from the Request. Keep :id implicit; the\n"+
			"        wrapper handles it via SetPathID.\n"+
			"    (2) For a compound route, use CommandWithBody / QueryWithParams\n"+
			"        instead, declare every segment with `path:\"...\"`, and call\n"+
			"        cmd.SetPathID(req.SomeField) from ToCommand / ToQuery.\n",
		wrapperName, wrapperName, reqType.String(),
	)
}

// warnGroupAMissingPathTag emits the §4.2 boot WARNING when a Group A
// wrapper (no :id auto-bind) is paired with a handler that calls
// RequirePathID at runtime AND the Request DTO declares no `path:` tag at
// all. Catches the misconfiguration where the consumer expected the wrapper
// to populate the path ID but neither the wrapper nor the Request provides
// it. The wrapper does not know the registered URL pattern at construction
// time, so the message identifies the wire-up by wrapper + handler +
// request types — sufficient to locate the call site.
func warnGroupAMissingPathTag(handler any, wrapperName string, reqType reflect.Type, schema *pathSchema) {
	if _, ok := handler.(pipeline.PathIDRequiredEnforcer); !ok {
		return
	}
	if schema != nil && len(schema.fields) > 0 {
		return
	}
	slog.Warn(
		"handler wired without an obvious path-ID source",
		slog.String("wrapper", wrapperName),
		slog.String("handler", reflect.TypeOf(handler).String()),
		slog.String("request", reqType.String()),
		slog.String("hint", "use a :id-binding wrapper, add path:\"someName\" on the Request and call cmd.SetPathID(req.SomeField) in ToCommand, or ignore this warning if the path ID is populated from a non-URL source (JWT, header, computed)"),
	)
}

// respondMissingFieldsAsSchema emits RequiredFieldNotification per missing
// field carrying SemanticSchema (instead of the domain default Validation).
// statusFromNotifications maps Schema → 400 — the status param of
// RespondFromResult is ignored when the Failure declares non-Validation
// semantic. Context = "Schema" to distinguish wire-level from domain-level.
func respondMissingFieldsAsSchema[TRes any](c fiber.Ctx, pipe *pipeline.Pipeline, missing []string) error {
	ctx := domain.NewNotificationContext("Schema")
	schema := domain.SemanticSchema
	for _, field := range missing {
		ctx.AddNotificationMessage(domain.NotificationMessage{
			FieldName:    field,
			Notification: domain.RequiredFieldNotification{}.WithSemantic(schema),
		})
	}
	err := domain.NewDomainError([]*domain.NotificationContext{ctx})
	result := pipeline.Run(pipe, AppContext(c), func() (TRes, error) {
		var zero TRes
		return zero, err
	})
	return RespondFromResult(c, result, fiber.StatusOK)
}

// respondSchemaViolation emits a single SchemaViolationNotification. field is
// the JSON path of the error (e.g. "addresses[0].zipCode" in case of type
// mismatch) or "" when the entire body is malformed. Always 400.
func respondSchemaViolation[TRes any](c fiber.Ctx, pipe *pipeline.Pipeline, field string) error {
	ctx := domain.NewNotificationContext("Schema")
	ctx.AddNotificationMessage(domain.NotificationMessage{
		FieldName:    field,
		Notification: domain.SchemaViolationNotification{},
	})
	err := domain.NewDomainError([]*domain.NotificationContext{ctx})
	result := pipeline.Run(pipe, AppContext(c), func() (TRes, error) {
		var zero TRes
		return zero, err
	})
	return RespondFromResult(c, result, fiber.StatusOK)
}

// extractBodyParserField tries to extract the field name from the BodyParser
// error. When the error is *json.UnmarshalTypeError, returns typeErr.Field
// (JSON path typically already in wire format — "addresses[0].zipCode").
// Other errors (SyntaxError, etc.) return "" indicating a malformed body
// without a specific field.
func extractBodyParserField(err error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return typeErr.Field
	}
	return ""
}
