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
	"fmt"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VpcEndpointServiceReconciler reconciles a VpcEndpointService object
type VpcEndpointServiceReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=vpc-endpoint-service.fil.com,resources=vpcendpointservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vpc-endpoint-service.fil.com,resources=vpcendpointservices/status,verbs=get;update;patch

func (r *VpcEndpointServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	fmt.Printf("%#v\n", req)
	return ctrl.Result{}, nil
}

func (r *VpcEndpointServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	controller := ctrl.NewControllerManagedBy(mgr).For(&v1.Service{}).Complete(r)
	return controller
}
