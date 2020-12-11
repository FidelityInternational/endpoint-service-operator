/*


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

package controllers

import (
	"context"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vpcendpointservicev1alpha1 "github.com/FidelityInternational/endpoint-service-operator/api/v1alpha1"
)

// VpcEndpointServiceReconciler reconciles a VpcEndpointService object
type VpcEndpointServiceReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=vpc-endpoint-service.fil.com,resources=vpcendpointservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vpc-endpoint-service.fil.com,resources=vpcendpointservices/status,verbs=get;update;patch

func (r *VpcEndpointServiceReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	_ = r.Log.WithValues("vpcendpointservice", req.NamespacedName)

	// your logic here

	return ctrl.Result{}, nil
}

func (r *VpcEndpointServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vpcendpointservicev1alpha1.VpcEndpointService{}).
		Complete(r)
}
