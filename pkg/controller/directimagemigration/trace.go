/*
Copyright 2020 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package directimagemigration

import (
	migapi "github.com/konveyor/mig-controller/pkg/apis/migration/v1alpha1"
	"github.com/konveyor/mig-controller/pkg/settings"
	migtrace "github.com/konveyor/mig-controller/pkg/tracing"
	"github.com/opentracing/opentracing-go"
)

func (r *ReconcileDirectImageMigration) initTracer(dim *migapi.DirectImageMigration) opentracing.Span {
	// Exit if tracing disabled
	if !settings.Settings.JaegerOpts.Enabled {
		return nil
	}
	// Set tracer on reconciler if it's not already present.
	// We will close spans, but not the tracer, so the 'closer' is discarded.
	if r.tracer == nil {
		r.tracer, _ = migtrace.InitJaeger("DirectImageMigration")
	}

	// Get overall migration span
	var migrationSpan opentracing.Span
	ownerRefs := dim.GetOwnerReferences()
	for _, ownerRef := range ownerRefs {
		if ownerRef.Kind != "MigMigration" {
			continue
		}
		migrationUID := string(ownerRef.UID)
		migrationSpan = migtrace.GetSpanForMigrationUID(migrationUID)
	}
	if migrationSpan == nil {
		return nil
	}

	// Get span for current reconcile
	var reconcileSpan opentracing.Span
	if migrationSpan != nil {
		reconcileSpan = r.tracer.StartSpan(
			"dim-reconcile-"+dim.Name, opentracing.ChildOf(migrationSpan.Context()),
		)
	}

	return reconcileSpan
}
