package bootstrap

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/infra"
)

// startUpstreamSubscribers spins one infra.UpstreamSubscriber goroutine
// per declared subscription, wired with its recompose-ripple targets
// (every view embedding the subscription's Collection via FromMongo).
//
// Lifecycle: the subscribers share the same ctx as SyncEngine + HTTP
// server, so SIGINT/SIGTERM cancels them in parallel. Each subscriber
// drains its workers and closes its Kafka reader on ctx.Done(), the same
// shape SyncEngine.run uses.
//
// The composer is built with NewComposerWithMongo so views embedding
// upstream-projected collections resolve correctly during the recompose
// ripple. One composer is shared across subscribers since it is
// stateless (pool + handle only).
func startUpstreamSubscribers(
	ctx context.Context,
	deps Deps,
	cfg *Config,
	subs []UpstreamSubscription,
	views []*infra.ViewDefinition,
) {
	if len(subs) == 0 {
		return
	}
	composer := infra.NewComposerWithMongo(deps.Postgres, deps.Mongo)
	for _, s := range subs {
		runtimeCfg := infra.UpstreamSubscriberConfig{
			Topic:            s.Topic,
			Collection:       s.Collection,
			ConsumerGroup:    s.ConsumerGroup,
			Workers:          s.Workers,
			Filter:           s.Filter,
			DeleteOnArchive:  s.DeleteOnArchive,
			StartFrom:        string(s.StartFrom),
			OnUpstreamDelete: string(s.OnUpstreamDelete),
			AnonymizeFields:  s.AnonymizeFields,
		}
		dependents := infra.DependentMongoViews(views, s.Collection)
		sub, err := infra.NewUpstreamSubscriber(
			deps.Postgres,
			deps.Mongo,
			composer,
			runtimeCfg,
			dependents,
			cfg.Kafka.Brokers,
			deps.Logger,
		)
		if err != nil {
			// Constructor only fails on structural errors that
			// boot guards already caught; log and skip so other
			// subscriptions still start.
			deps.Logger.Error("bootstrap: upstream subscriber init failed",
				"topic", s.Topic, "err", err)
			continue
		}
		sub.Start(ctx)
		deps.Logger.Info("upstream subscriber started",
			"topic", s.Topic,
			"collection", s.Collection,
			"consumerGroup", s.ConsumerGroup,
			"workers", s.Workers,
			"dependentViews", len(dependents),
			"onUpstreamDelete", s.OnUpstreamDelete,
			"deleteOnArchive", s.DeleteOnArchive,
			"startFrom", s.StartFrom)
	}
}
