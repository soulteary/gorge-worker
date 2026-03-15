package handlers

import (
	"github.com/soulteary/gorge-worker/internal/worker"
)

// RegisterAll registers the built-in task handlers into the registry.
// Additional handlers can be registered by the caller after this function.
func RegisterAll(registry *worker.Registry, conduitURL, conduitToken string) {
	// Conduit-delegated handlers: these re-delegate task execution to the
	// Phorge PHP backend via Conduit API calls, allowing github.com/soulteary/gorge-worker to serve
	// as a task consumer without reimplementing complex PHP business logic.
	if conduitURL != "" {
		conduit := NewConduitClient(conduitURL, conduitToken)

		// Feed publisher — needs to load FeedStory from DB and render text,
		// so it must be delegated to PHP.
		registry.Register("FeedPublisherHTTPWorker", NewConduitDelegateHandler(conduit))

		// Search indexer — depends on PhabricatorIndexEngine and multiple
		// extension classes in PHP.
		registry.Register("PhabricatorSearchWorker", NewConduitDelegateHandler(conduit))

		// Mail sender — depends on PhabricatorMetaMTAMail and mail adapters.
		registry.Register("PhabricatorMetaMTAWorker", NewConduitDelegateHandler(conduit))

		// Transaction publisher — complex editor/notification/feed logic.
		registry.Register("PhabricatorApplicationTransactionPublishWorker", NewConduitDelegateHandler(conduit))

		// Herald webhook — can be handled by github.com/soulteary/gorge-webhook directly,
		// but also support delegation for deployments without github.com/soulteary/gorge-webhook.
		registry.Register("HeraldWebhookWorker", NewConduitDelegateHandler(conduit))
	}
}
