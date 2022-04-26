/*
Copyright 2021.

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

package configauditreport

import (
	"context"
	"fmt"

	aqua "github.com/aquasecurity/starboard/pkg/apis/aquasecurity/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/giantswarm/starboard-exporter/controllers"
	"github.com/giantswarm/starboard-exporter/utils"
)

const (
	ConfigAuditReportFinalizer = "starboard-exporter.giantswarm.io/configauditreport"
)

// ConfigAuditReportReconciler reconciles a ConfigAuditReport object
type ConfigAuditReportReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme

	MaxJitterPercent int
}

//+kubebuilder:rbac:groups=aquasecurity.github.io.giantswarm,resources=configauditreports,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=aquasecurity.github.io.giantswarm,resources=configauditreports/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=aquasecurity.github.io.giantswarm,resources=configauditreports/finalizers,verbs=update
func (r *ConfigAuditReportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)
	_ = r.Log.WithValues("configauditreport", req.NamespacedName)

	report := &aqua.ConfigAuditReport{}
	if err := r.Client.Get(ctx, req.NamespacedName, report); err != nil {
		if apierrors.IsNotFound(err) {
			// Most likely the report was deleted.
			return ctrl.Result{}, nil
		}

		// Error reading the object.
		r.Log.Error(err, "Unable to read configauditreport")
		return ctrl.Result{}, err
	}

	if report.DeletionTimestamp.IsZero() {
		// Give the report our finalizer if it doesn't have one.
		if !utils.SliceContains(report.GetFinalizers(), ConfigAuditReportFinalizer) {
			ctrlutil.AddFinalizer(report, ConfigAuditReportFinalizer)
			if err := r.Update(ctx, report); err != nil {
				return ctrl.Result{}, err
			}
		}

		r.Log.Info(fmt.Sprintf("Reconciled %s || Found (C/H/M/L): %d/%d/%d/%d",
			req.NamespacedName,
			report.Report.Summary.CriticalCount,
			report.Report.Summary.HighCount,
			report.Report.Summary.MediumCount,
			report.Report.Summary.LowCount,
		))

		// Publish summary metrics for this report.
		publishSummaryMetrics(report)

	} else {

		if utils.SliceContains(report.GetFinalizers(), ConfigAuditReportFinalizer) {
			// Unfortunately, we can't just clear the series based on one label value,
			// we have to reconstruct all of the label values to delete the series.
			// That's the only reason the finalizer is needed at all.
			r.clearImageMetrics(report)

			ctrlutil.RemoveFinalizer(report, ConfigAuditReportFinalizer)
			if err := r.Update(ctx, report); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	return utils.JitterRequeue(controllers.DefaultRequeueDuration, r.MaxJitterPercent, r.Log), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ConfigAuditReportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&aqua.ConfigAuditReport{}).
		Complete(r)
	if err != nil {
		return errors.Wrap(err, "failed setting up controller with controller manager")
	}

	return nil
}

func (r *ConfigAuditReportReconciler) clearImageMetrics(report *aqua.ConfigAuditReport) {
	// clear summary metrics
	summaryValues := valuesForReport(report, metricLabels)

	// Delete the series for each severity.
	for severity := range getCountPerSeverity(report) {
		v := summaryValues
		v["severity"] = severity

		// Delete the metric.
		ConfigAuditSummary.Delete(
			v,
		)
	}
}

func getCountPerSeverity(report *aqua.ConfigAuditReport) map[string]float64 {
	// Format is e.g. {CRITICAL: 10}.
	return map[string]float64{
		string(aqua.SeverityCritical): float64(report.Report.Summary.CriticalCount),
		string(aqua.SeverityHigh):     float64(report.Report.Summary.HighCount),
		string(aqua.SeverityMedium):   float64(report.Report.Summary.MediumCount),
		string(aqua.SeverityLow):      float64(report.Report.Summary.LowCount),
	}
}

func publishSummaryMetrics(report *aqua.ConfigAuditReport) {
	summaryValues := valuesForReport(report, metricLabels)

	// Add the severity label after the standard labels and expose each severity metric.
	for severity, count := range getCountPerSeverity(report) {
		v := summaryValues
		v["severity"] = severity

		// Expose the metric.
		ConfigAuditSummary.With(
			v,
		).Set(count)
	}
}

func valuesForReport(report *aqua.ConfigAuditReport, labels []string) map[string]string {
	result := map[string]string{}
	for _, label := range labels {
		result[label] = reportValueFor(label, report)
	}
	return result
}

func reportValueFor(field string, report *aqua.ConfigAuditReport) string {
	switch field {
	case "resource_name":
		return report.Name
	case "resource_namespace":
		return report.Namespace
	case "severity":
		return "" // this value will be overwritten on publishSummaryMetrics
	default:
		// Error?
		return ""
	}
}