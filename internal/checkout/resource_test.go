package checkout_test

import (
	"go.opentelemetry.io/otel/attribute"
)

func serviceName(name string) attribute.KeyValue { return attribute.String("service.name", name) }
