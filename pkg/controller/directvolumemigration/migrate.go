package directvolumemigration

import (
	"context"
	"errors"
	"fmt"
	"time"

	liberr "github.com/konveyor/controller/pkg/error"
	"github.com/konveyor/mig-controller/pkg/errorutil"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"

	migapi "github.com/konveyor/mig-controller/pkg/apis/migration/v1alpha1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (r *ReconcileDirectVolumeMigration) migrate(ctx context.Context, direct *migapi.DirectVolumeMigration) (time.Duration, error) {

	planResources, err := r.getDVMPlanResources(direct)
	if err != nil {
		return 0, liberr.Wrap(err)
	}

	sparseFilePVCMap, err := r.getSparseFilePVCMap(planResources.MigPlan)
	if err != nil {
		return 0, liberr.Wrap(err)
	}

	endpointType, err := r.getEndpointType(direct)
	if err != nil {
		return 0, liberr.Wrap(err)
	}

	// Started
	if direct.Status.StartTimestamp == nil {
		log.Info("Marking DirectVolumeMigration as started.")
		direct.Status.StartTimestamp = &metav1.Time{Time: time.Now()}
	}

	// Run
	task := Task{
		Log:              log,
		Client:           r,
		Owner:            direct,
		Phase:            direct.Status.Phase,
		PhaseDescription: direct.Status.PhaseDescription,
		PlanResources:    planResources,
		SparseFileMap:    sparseFilePVCMap,
		Tracer:           r.tracer,
		EndpointType:     endpointType,
	}
	err = task.Run(ctx)
	if err != nil {
		if k8serrors.IsConflict(errorutil.Unwrap(err)) {
			log.V(4).Info("Conflict error during task.Run, requeueing.")
			return FastReQ, nil
		}
		log.Info("Phase execution failed.",
			"phase", task.Phase,
			"phaseDescription", task.getPhaseDescription(task.Phase),
			"error", errorutil.Unwrap(err).Error())
		sink.Trace(err)
		if errors.Is(err, FatalPlanError) {
			task.fail(MigrationFailed, Critical, []string{err.Error()})
		} else {
			task.fail(MigrationFailed, Warn, []string{err.Error()})
		}
	}

	// Result
	direct.Status.PhaseDescription = task.PhaseDescription
	direct.Status.Phase = task.Phase
	direct.Status.Itinerary = task.Itinerary.Name

	// Completed
	if task.Phase == Completed {
		direct.Status.DeleteCondition(Running)
		failed := task.Owner.Status.FindCondition(Failed)
		if failed == nil {
			direct.Status.SetCondition(migapi.Condition{
				Type:     Succeeded,
				Status:   True,
				Reason:   task.Phase,
				Category: Advisory,
				Message:  SucceededMessage,
				Durable:  true,
			})
		}
		return NoReQ, nil
	}

	// Running
	step, n, total := task.Itinerary.progressReport(task.Phase)
	message := fmt.Sprintf(RunningMessage, n, total)
	direct.Status.SetCondition(migapi.Condition{
		Type:     Running,
		Status:   True,
		Reason:   step,
		Category: Advisory,
		Message:  message,
	})

	return task.Requeue, nil
}

// fetches DVM Migration object and Migplan resources if DVM has an owner reference
func (r *ReconcileDirectVolumeMigration) getDVMPlanResources(direct *migapi.DirectVolumeMigration) (*migapi.PlanResources, error) {

	if len(direct.OwnerReferences) > 0 {

		migration := &migapi.MigMigration{}
		planResources := &migapi.PlanResources{}

		// Ready
		migration, err := direct.GetMigrationForDVM(r)
		if err != nil {
			return planResources, liberr.Wrap(err)
		}

		if migration == nil {
			log.Info("Migration not found for DVM", "name", direct.Name)
			return planResources, liberr.Wrap(err)
		}

		plan, err := migration.GetPlan(r)
		if err != nil {
			return planResources, liberr.Wrap(err)
		}
		if !plan.Status.IsReady() {
			log.Info("Plan not ready.", "name", migration.Name)
			return planResources, liberr.Wrap(err)
		}

		// Resources
		planResources, err = plan.GetRefResources(r)
		if err != nil {
			return planResources, liberr.Wrap(err)
		}
		return planResources, nil
	}
	return &migapi.PlanResources{}, nil
}

type sparseFilePVCMap map[string]bool

func (r *ReconcileDirectVolumeMigration) getSparseFilePVCMap(plan *migapi.MigPlan) (sparseFilePVCMap, error) {
	sparseFilesMap := make(sparseFilePVCMap)
	if plan == nil {
		return sparseFilesMap, nil
	}
	analytics := &migapi.MigAnalyticList{}
	err := r.List(context.TODO(),
		analytics, k8sclient.MatchingLabels(
			map[string]string{
				"migplan": plan.Name}))
	if err != nil {
		return nil, liberr.Wrap(err)
	}
	for _, migAnalytic := range analytics.Items {
		if migAnalytic.Spec.AnalyzeExtendedPVCapacity {
			for _, ns := range migAnalytic.Status.Analytics.Namespaces {
				for _, pv := range ns.PersistentVolumes {
					if pv.SparseFilesFound {
						sparseFilesMap[fmt.Sprintf(
							"%s/%s", ns.Namespace, pv.Name)] = true
					}
				}
			}
		}
	}
	return sparseFilesMap, nil
}

// getEndpointType returns user configured endpoint type to be used for rsync transfer
func (r *ReconcileDirectVolumeMigration) getEndpointType(direct *migapi.DirectVolumeMigration) (migapi.EndpointType, error) {
	destCluster, err := direct.GetDestinationCluster(r)
	if err != nil {
		return "", liberr.Wrap(err)
	}
	destClient, err := destCluster.GetClient(r)
	if err != nil {
		return "", liberr.Wrap(err)
	}
	clusterConfig, err := destCluster.GetClusterConfigMap(destClient)
	if err != nil {
		return "", liberr.Wrap(err)
	}
	endpointType, exists := clusterConfig.Data[migapi.RSYNC_ENDPOINT_TYPE]
	if !exists {
		return migapi.Route, nil
	}
	switch migapi.EndpointType(endpointType) {
	case migapi.Route, migapi.ClusterIP, migapi.NodePort:
		return migapi.EndpointType(endpointType), nil
	default:
		log.Info("invalid endpoint type specified, using default", "specified", endpointType, "default", migapi.Route)
		return migapi.Route, nil
	}
}
