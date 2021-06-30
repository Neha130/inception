/*
Copyright 2020 Devtron Labs Pvt Ltd.

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
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/antlr/antlr4/runtime/Go/antlr"
	installerv1alpha1 "github.com/devtron-labs/inception/api/v1alpha1"
	"github.com/devtron-labs/inception/pkg/language"
	parser2 "github.com/devtron-labs/inception/pkg/language/parser"
	"github.com/go-logr/logr"
	"github.com/patrickmn/go-cache"
	"github.com/posthog/posthog-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"io"
	"io/ioutil"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"net/http"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"time"
)

// InstallerReconciler reconciles a Installer object
type InstallerReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
	//Instead of KLangListener
	Mapper        *language.Mapper
	PosthogClient posthog.Client
	Cache         *cache.Cache
}

const DevtronUniqueClientIdConfigMap = "devtron-ucid"
const DevtronUniqueClientIdConfigMapKey = "UCID"
const DevtronNamespace = "devtroncd"

type TelemetryEventType int

const (
	Heartbeat              string = "Heartbeat"
	InstallationStart      string = "InstallationStart"
	InstallationInProgress string = "InstallationInProgress"
	InstallationSuccess    string = "InstallationSuccess"
	InstallationFailure    string = "InstallationFailure"
	UpgradeStart           string = "UpgradeStart"
	UpgradeInProgress      string = "UpgradeInProgress"
	UpgradeSuccess         string = "UpgradeSuccess"
	UpgradeFailure         string = "UpgradeFailure"
	Summary                string = "Summary"
)

type TelemetryEventDto struct {
	UCID           string    `json:"ucid"` //unique client id
	Timestamp      time.Time `json:"timestamp"`
	EventMessage   string    `json:"eventMessage,omitempty"`
	EventType      string    `json:"eventType"`
	ServerVersion  string    `json:"serverVersion,omitempty"`
	DevtronVersion string    `json:"devtronVersion,omitempty"`
}

// +kubebuilder:rbac:groups=installer.devtron.ai,resources=installers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=installer.devtron.ai,resources=installers/status,verbs=get;update;patch

func (r *InstallerReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	_ = r.Log.WithValues("installer", req.NamespacedName)

	installer := &installerv1alpha1.Installer{}
	err := r.Client.Get(context.Background(), req.NamespacedName, installer)
	if err != nil {
		//Dont requeue if it is not found
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		//This will requeue the request
		return reconcile.Result{}, err
	}

	if len(installer.Spec.URL) == 0 {
		return reconcile.Result{}, fmt.Errorf("url is not specified")
	}
	updated := false

	var payload *TelemetryEventDto
	installEvent := false
	UCID, cm, err := r.getUCID(false)
	if err != nil {
		//TODO: this is not failed event
		r.Log.Error(err, "failed to send event to posthog")
	}
	if cm != nil && cm.Data != nil {
		dataMap := cm.Data
		status := dataMap["status"]
		if status == InstallationStart || status == InstallationInProgress ||
			status == InstallationFailure {
			installEvent = true
		}
	}
	if hasSpecChanged(installer) {
		fmt.Println("url changed")
		installer.Status.Sync.Status = installerv1alpha1.SyncStatusCodeOutOfSync
		installer.Status.Sync.StatusMessage = installerv1alpha1.SyncStatusMessageOutOfSync
		installer.Status.Sync.URL = installer.Spec.URL
		installer.Spec.ReSync = false
		updated = true
		if len(UCID) == 0 || installEvent {
			payload = &TelemetryEventDto{UCID: UCID, Timestamp: time.Now(), EventType: InstallationStart, DevtronVersion: "v1"}
		} else {
			payload = &TelemetryEventDto{UCID: UCID, Timestamp: time.Now(), EventType: UpgradeStart, DevtronVersion: "v1"}
		}
	} else if shouldDownload(installer) {
		fmt.Println("should download")
		err := r.sync(installer)
		if err != nil {
			return reconcile.Result{}, err
		}
		updated = true
		if len(UCID) == 0 || installEvent {
			payload = &TelemetryEventDto{UCID: UCID, Timestamp: time.Now(), EventType: InstallationInProgress, DevtronVersion: "v1"}
		} else {
			payload = &TelemetryEventDto{UCID: UCID, Timestamp: time.Now(), EventType: UpgradeInProgress, DevtronVersion: "v1"}
		}
	} else if shouldApply(installer) {
		fmt.Println("applying")
		r.apply(installer)
		updated = true
		if len(UCID) == 0 || installEvent {
			payload = &TelemetryEventDto{UCID: UCID, Timestamp: time.Now(), EventType: InstallationInProgress, DevtronVersion: "v1"}
		} else {
			payload = &TelemetryEventDto{UCID: UCID, Timestamp: time.Now(), EventType: UpgradeInProgress, DevtronVersion: "v1"}
		}
	}

	//TODO - setup correct event trigger points
	if updated {
		fmt.Println("updating")
		err = r.Client.Update(context.Background(), installer)
		if err != nil {
			if len(UCID) == 0 || installEvent {
				payload = &TelemetryEventDto{UCID: UCID, Timestamp: time.Now(), EventType: InstallationFailure, DevtronVersion: "v1"}
			} else {
				payload = &TelemetryEventDto{UCID: UCID, Timestamp: time.Now(), EventType: UpgradeFailure, DevtronVersion: "v1"}
			}
			err = r.sendEvent(payload)
			if err != nil {
				r.Log.Error(err, "failed to send event to posthog")
			}
			err = r.updateStatusOnCm(cm, payload.EventType)
			if err != nil {
				r.Log.Error(err, "failed to update cm")
			}
			return reconcile.Result{}, err
		} else {
			if len(UCID) == 0 {
				payload = &TelemetryEventDto{UCID: UCID, Timestamp: time.Now(), EventType: InstallationSuccess, DevtronVersion: "v1"}
			} else {
				payload = &TelemetryEventDto{UCID: UCID, Timestamp: time.Now(), EventType: UpgradeSuccess, DevtronVersion: "v1"}
			}
		}
	}
	err = r.sendEvent(payload)
	if err != nil {
		r.Log.Error(err, "failed to send event to posthog")
	}
	err = r.updateStatusOnCm(cm, payload.EventType)
	if err != nil {
		r.Log.Error(err, "failed to update cm")
	}
	return ctrl.Result{}, nil
}

func (r *InstallerReconciler) sendEvent(payload *TelemetryEventDto) error {
	prop := make(map[string]interface{})
	//payload := &TelemetryEventDto{UCID: "ucid", Timestamp: time.Now(), EventType: Summary, DevtronVersion: "v1"}
	reqBody, err := json.Marshal(payload)
	if err != nil {
		r.Log.Error(err, "telemetry event to posthog from operator, payload marshal error")
		return nil
	}
	err = json.Unmarshal(reqBody, &prop)
	if err != nil {
		r.Log.Error(err, "telemetry event to posthog from operator, payload unmarshal error")
		return nil
	}
	if r.PosthogClient != nil {
		r.PosthogClient.Enqueue(posthog.Capture{
			DistinctId: payload.UCID,
			Event:      payload.EventType,
			Properties: prop,
		})
	}
	return nil
}

func (r *InstallerReconciler) sync(installer *installerv1alpha1.Installer) error {
	//Download from url
	url := installer.Status.Sync.URL
	data, err := downloadDSL(url)
	if err != nil {
		return err
	}
	//Update url on which action was taken to handle race condition
	installer.Status.Sync.URL = url
	installer.Status.Sync.Data = data
	installer.Status.Sync.Status = installerv1alpha1.SyncStatusCodeDownloaded
	installer.Status.Sync.StatusMessage = installerv1alpha1.SyncStatusMessageDownloaded
	return nil
}

func (r *InstallerReconciler) apply(installer *installerv1alpha1.Installer) *language.KlangListener {
	url := installer.Status.Sync.URL
	data := installer.Status.Sync.Data
	//parse and process data
	is := antlr.NewInputStream(data)
	lexer := parser2.NewKlangLexer(is)
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	p := parser2.NewKlangParser(stream)
	p.BuildParseTrees = true
	listener := language.NewKlangListener(r.Mapper)
	antlr.ParseTreeWalkerDefault.Walk(listener, p.Parse())
	//TODO: Use r.Parser.Values() to check data and get resources
	//Update the status of resources
	var resourceStatuses []installerv1alpha1.ResourceStatus
	for _, resources := range listener.KubernetesResources() {
		for _, resource := range resources {
			rs := installerv1alpha1.ResourceStatus{
				Group:     resource.Group,
				Version:   resource.Version,
				Kind:      resource.Kind,
				Namespace: resource.Namespace,
				Name:      resource.Name,
				Status:    toSyncStatus(resource.Status),
				Health:    nil,
				Operation: resource.Operation,
				Message:   resource.Message,
			}
			resourceStatuses = append(resourceStatuses, rs)
		}
	}
	//Update URL and data on which action was taken to handle race condition
	installer.Status.Sync.URL = url
	installer.Status.Sync.Data = data
	installer.Status.Sync.Status = installerv1alpha1.SyncStatusCodeApplied
	installer.Status.Sync.StatusMessage = installerv1alpha1.SyncStatusMessageApplied
	installer.Status.Sync.Resources = resourceStatuses
	return listener
}

func toSyncStatus(code language.ResourceSyncStatusCode) installerv1alpha1.SyncStatusCode {
	switch code {
	case language.ResourceSyncStatusCodeOutOfSync:
		return installerv1alpha1.SyncStatusCodeOutOfSync
	case language.ResourceSyncStatusCodeSynced:
		return installerv1alpha1.SyncStatusCodeSynced
	case language.ResourceSyncStatusCodeUnknown:
		return installerv1alpha1.SyncStatusCodeUnknown
	default:
		return installerv1alpha1.SyncStatusCodeUnknown
	}
}

func downloadDSL(url string) (string, error) {
	hasher := sha1.New()
	hasher.Write([]byte(url))
	hash := hex.EncodeToString(hasher.Sum(nil))
	fileName := fmt.Sprintf("/tmp/%s", hash)
	file, err := os.Create(fileName)
	if err != nil {
		return "", err
	}

	defer func() {
		err := file.Close()
		if err != nil {
			//Log
		}
	}()

	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return "", err
	}

	data, err := ioutil.ReadFile(fileName)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func hasSpecChanged(installer *installerv1alpha1.Installer) bool {
	if installer.Spec.URL != installer.Status.Sync.URL || installer.Spec.ReSync {
		return true
	}
	return false
}

func shouldDownload(installer *installerv1alpha1.Installer) bool {
	if installer.Status.Sync.Status == installerv1alpha1.SyncStatusCodeOutOfSync {
		return true
	}
	return false
}

func shouldApply(installer *installerv1alpha1.Installer) bool {
	if installer.Status.Sync.Status == installerv1alpha1.SyncStatusCodeDownloaded {
		return true
	}
	return false
}

func (r *InstallerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&installerv1alpha1.Installer{}).
		Complete(r)
}

func (r *InstallerReconciler) GetClientForInCluster() (*v12.CoreV1Client, error) {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		r.Log.Error(err, " error on in cluster config")
		return nil, err
	}
	// creates the clientset
	clientset, err := v12.NewForConfig(config)
	if err != nil {
		r.Log.Error(err, " error on in cluster config client")
		return nil, err
	}
	return clientset, err
}

func (r *InstallerReconciler) GetConfigMap(namespace string, name string, client *v12.CoreV1Client) (*v1.ConfigMap, error) {
	ctx := context.Background()
	cm, err := client.ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	} else {
		return cm, nil
	}
}

func (r *InstallerReconciler) CreateConfigMap(namespace string, cm *v1.ConfigMap, client *v12.CoreV1Client) (*v1.ConfigMap, error) {
	ctx := context.Background()
	cm, err := client.ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	} else {
		return cm, nil
	}
}

func (r *InstallerReconciler) UpdateConfigMap(namespace string, cm *v1.ConfigMap, client *v12.CoreV1Client) (*v1.ConfigMap, error) {
	ctx := context.Background()
	cm, err := client.ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	} else {
		return cm, nil
	}
}

func (r *InstallerReconciler) updateStatusOnCm(cm *v1.ConfigMap, eventType string) error {
	client, err := r.GetClientForInCluster()
	if err != nil {
		r.Log.Error(err, "exception while update updateStatusOnCm")
		return err
	}
	dataMap := cm.Data
	dataMap["status"] = eventType
	cm.Data = dataMap
	_, err = r.UpdateConfigMap(DevtronNamespace, cm, client)
	if err != nil {
		r.Log.Error(err, "exception while update updateStatusOnCm")
		return err
	}
	return nil
}

func (r *InstallerReconciler) getUCID(fromCache bool) (string, *v1.ConfigMap, error) {
	var cm *v1.ConfigMap
	ucid, found := r.Cache.Get(DevtronUniqueClientIdConfigMapKey)
	//TODO: refactor the code to include only one if condition i.e; if !found {
	if found && fromCache {
		return ucid.(string), cm, nil
	} else {
		//TODO: use r.Client instead of creating new client
		client, err := r.GetClientForInCluster()
		if err != nil {
			r.Log.Error(err, "exception while getting unique client id")
			return ucid.(string), cm, nil
		}
		cm, err = r.GetConfigMap(DevtronNamespace, DevtronUniqueClientIdConfigMap, client)
		if errStatus, ok := status.FromError(err); !ok || errStatus.Code() == codes.NotFound || errStatus.Code() == codes.Unknown {
			// if not found, create new cm
			cm = &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: DevtronUniqueClientIdConfigMap}}
			data := map[string]string{}
			//TODO: we can rather use SHA1 to generate uniqueId, that has low chances of collision
			data[DevtronUniqueClientIdConfigMapKey] = language.Generate(16) // generate unique random number
			cm.Data = data
			_, err = r.CreateConfigMap(DevtronNamespace, cm, client)
			if err != nil {
				r.Log.Error(err, "exception while getting unique client id")
				return ucid.(string), cm, nil
			}
		}
		dataMap := cm.Data
		ucid = dataMap[DevtronUniqueClientIdConfigMapKey]
		r.Cache.Set(DevtronUniqueClientIdConfigMapKey, ucid, cache.DefaultExpiration)
		//TODO: should be cm != nil
		if cm == nil {
			r.Log.Error(err, "configmap not found while getting unique client id", "cm", cm)
			return ucid.(string), cm, nil
		}
	}
	return ucid.(string), cm, nil
}
