package query

// Field-ownership discipline for CONSULT-class view documents (SharedBaseView
// and any view with external embeds). Two independent consumers converge on
// the same document _id with full recomposes:
//
//   - the SyncEngine (entity/role/base events) — its relational read is fresh,
//     but the embed segments it composed came from the local mirror collection
//     at read time and may predate a concurrent upstream event;
//   - the UpstreamSubscriber recompose-ripple (upstream events) — its mirror
//     read is fresh, but the relational fields may predate a concurrent write
//     still being processed by the SyncEngine.
//
// A plain full-document Upsert makes the LAST writer win on every field, so
// the loser's fresh fields are regressed by the winner's stale ones — a
// classic lost-update (observed as an EmbedMany array frozen without its
// newest item whenever the parent's own INSERT event lands after the item
// ripples; the wider the CDC batching skew between the two topics, the more
// likely — Oracle LogMiner's mining cadence hits it reliably). The fix is
// ownership, not timing: each writer writes ONLY the fields it read freshly
// and leaves the other side's fields untouched — unless the document does not
// exist yet, in which case whoever arrives first materializes the full
// composition (the other writer's next update then only touches its own
// fields). Either way the write remains ONE atomic pipeline upsert.
func fieldOwnershipStages(doc Document, pkCol string, embedFields map[string]struct{}, ownsEmbeds bool) []Document {
	exists := Document{"$ne": []any{Document{"$type": "$" + pkCol}, "missing"}}
	set := Document{}
	for k, v := range doc {
		if k == "_id" {
			continue
		}
		_, isEmbed := embedFields[k]
		if isEmbed == ownsEmbeds {
			set[k] = lit(v)
			continue
		}
		// The other writer's field: keep the stored value; write the composed
		// one only when this upsert is creating the document. Existence is
		// probed via the root PK column — absent on a fresh upsert-insert,
		// present on every materialized document (composer and payload
		// projector both carry it).
		set[k] = Document{"$cond": []any{exists, "$" + k, lit(v)}}
	}
	return []Document{{"$set": set}}
}

// embedFieldSet returns the document segment names claimed by the recompose
// ripple — one per declared embed.
func embedFieldSet(embeds []embedDef) map[string]struct{} {
	if len(embeds) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(embeds))
	for _, e := range embeds {
		out[e.Field()] = struct{}{}
	}
	return out
}
