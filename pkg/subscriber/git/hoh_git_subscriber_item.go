// Copyright 2021 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package git

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	chnv1 "open-cluster-management.io/multicloud-operators-channel/pkg/apis/apps/v1"
	appv1 "open-cluster-management.io/multicloud-operators-subscription/pkg/apis/apps/v1"
	"open-cluster-management.io/multicloud-operators-subscription/pkg/utils"
)

const sharedVolumePath = "/opt/hub-of-hubs-subscription-storage"

// HubOfHubsSubscriberItem - defines the unit of namespace subscription
type HubOfHubsSubscriberItem struct {
	appv1.SubscriberItem
	repoRoot       string
	commitID       string
	reconcileRate  string
	desiredCommit  string
	desiredTag     string
	syncTime       string
	stopch         chan struct{}
	syncinterval   int
	count          int
	synchronizer   SyncSource
	webhookEnabled bool
	successful     bool
	clusterAdmin   bool
	userID         string
	userGroup      string
}

// Start subscribes a subscriber item with github channel
func (ghsi *HubOfHubsSubscriberItem) Start(restart bool) {
	// do nothing if already started
	if ghsi.stopch != nil {
		if restart {
			// restart this goroutine
			klog.Info("Stopping SubscriberItem: ", ghsi.Subscription.Name)
			ghsi.Stop()
		} else {
			klog.Info("SubscriberItem already started: ", ghsi.Subscription.Name)
			return
		}
	}

	ghsi.count = 0 // reset the counter

	ghsi.stopch = make(chan struct{})

	loopPeriod, retryInterval, retries := utils.GetReconcileInterval(ghsi.reconcileRate, chnv1.ChannelTypeGit)

	if strings.EqualFold(ghsi.reconcileRate, "off") {
		klog.Infof("auto-reconcile is OFF")

		ghsi.doSubscriptionWithRetries(retryInterval, retries)

		return
	}

	go wait.Until(func() {
		tw := ghsi.SubscriberItem.Subscription.Spec.TimeWindow
		if tw != nil {
			nextRun := utils.NextStartPoint(tw, time.Now())
			if nextRun > time.Duration(0) {
				klog.Infof("Subscription is currently blocked by the time window. It %v/%v will be deployed after %v",
					ghsi.SubscriberItem.Subscription.GetNamespace(),
					ghsi.SubscriberItem.Subscription.GetName(), nextRun)

				return
			}
		}

		// if the subscription pause lable is true, stop subscription here.
		if utils.GetPauseLabel(ghsi.SubscriberItem.Subscription) {
			klog.Infof("Git Subscription %v/%v is paused.", ghsi.SubscriberItem.Subscription.GetNamespace(), ghsi.SubscriberItem.Subscription.GetName())

			return
		}

		ghsi.doSubscriptionWithRetries(retryInterval, retries)
	}, loopPeriod, ghsi.stopch)
}

// Stop unsubscribes a subscriber item with namespace channel
func (ghsi *HubOfHubsSubscriberItem) Stop() {
	klog.Info("Stopping SubscriberItem ", ghsi.Subscription.Name)
	close(ghsi.stopch)
}

func (ghsi *HubOfHubsSubscriberItem) doSubscriptionWithRetries(retryInterval time.Duration, retries int) {
	err := ghsi.doSubscription()

	if err != nil {
		klog.Error(err, "Subscription error.")
	}

	// If the initial subscription fails, retry.
	n := 0

	for n < retries {
		if !ghsi.successful {
			time.Sleep(retryInterval)
			klog.Infof("Re-try #%d: subscribing to the Git repo", n+1)

			err = ghsi.doSubscription()
			if err != nil {
				klog.Error(err, "Subscription error.")
			}

			n++
		} else {
			break
		}
	}
}

func (ghsi *HubOfHubsSubscriberItem) doSubscription() error {
	hostkey := types.NamespacedName{Name: ghsi.Subscription.Name, Namespace: ghsi.Subscription.Namespace}
	klog.Info("enter doSubscription: ", hostkey.String())

	defer klog.Info("exit doSubscription: ", hostkey.String())

	// If webhook is enabled, don't do anything until next reconciliation.
	if ghsi.webhookEnabled {
		klog.Infof("Git Webhook is enabled on subscription %s.", ghsi.Subscription.Name)

		if ghsi.successful {
			klog.Infof("All resources are reconciled successfully. Waiting for the next Git Webhook event.")
			return nil
		}

		klog.Infof("Resources are not reconciled successfully yet. Continue reconciling.")
	}

	klog.Info("Subscribing ...", ghsi.Subscription.Name)

	//Update the secret and config map
	if ghsi.Channel != nil {
		sec, cm := utils.FetchChannelReferences(ghsi.synchronizer.GetRemoteNonCachedClient(), *ghsi.Channel)
		if sec != nil {
			if err := utils.ListAndDeployReferredObject(ghsi.synchronizer.GetLocalNonCachedClient(), ghsi.Subscription,
				schema.GroupVersionKind{Group: "", Kind: "Secret", Version: "v1"}, sec); err != nil {
				klog.Warningf("can't deploy reference secret %v for subscription %v", ghsi.ChannelSecret.GetName(), ghsi.Subscription.GetName())
			}
		}

		if cm != nil {
			if err := utils.ListAndDeployReferredObject(ghsi.synchronizer.GetLocalNonCachedClient(), ghsi.Subscription,
				schema.GroupVersionKind{Group: "", Kind: "ConfigMap", Version: "v1"}, cm); err != nil {
				klog.Warningf("can't deploy reference configmap %v for subscription %v", ghsi.ChannelConfigMap.GetName(), ghsi.Subscription.GetName())
			}
		}

		sec, cm = utils.FetchChannelReferences(ghsi.synchronizer.GetLocalNonCachedClient(), *ghsi.Channel)
		if sec != nil {
			klog.V(1).Info("updated in memory channel secret for ", ghsi.Subscription.Name)
			ghsi.ChannelSecret = sec
		}

		if cm != nil {
			klog.V(1).Info("updated in memory channel configmap for ", ghsi.Subscription.Name)
			ghsi.ChannelConfigMap = cm
		}
	}

	if ghsi.SecondaryChannel != nil {
		sec, cm := utils.FetchChannelReferences(ghsi.synchronizer.GetRemoteNonCachedClient(), *ghsi.SecondaryChannel)
		if sec != nil {
			if err := utils.ListAndDeployReferredObject(ghsi.synchronizer.GetLocalNonCachedClient(), ghsi.Subscription,
				schema.GroupVersionKind{Group: "", Kind: "Secret", Version: "v1"}, sec); err != nil {
				klog.Warningf("can't deploy reference secondary secret %v for subscription %v", ghsi.SecondaryChannelSecret.GetName(), ghsi.Subscription.GetName())
			}
		}

		if cm != nil {
			if err := utils.ListAndDeployReferredObject(ghsi.synchronizer.GetLocalNonCachedClient(), ghsi.Subscription,
				schema.GroupVersionKind{Group: "", Kind: "ConfigMap", Version: "v1"}, cm); err != nil {
				klog.Warningf("can't deploy reference secondary configmap %v for subscription %v", ghsi.SecondaryChannelConfigMap.GetName(), ghsi.Subscription.GetName())
			}
		}

		sec, cm = utils.FetchChannelReferences(ghsi.synchronizer.GetLocalNonCachedClient(), *ghsi.SecondaryChannel)
		if sec != nil {
			klog.Info("updated in memory secondary channel secret for ", ghsi.Subscription.Name)
			ghsi.SecondaryChannelSecret = sec
		}

		if cm != nil {
			klog.V(1).Info("updated in memory secondary channel configmap for ", ghsi.Subscription.Name)
			ghsi.SecondaryChannelConfigMap = cm
		}
	}

	//Clone the git repo
	commitID, err := ghsi.cloneGitRepo()
	if err != nil {
		klog.Error(err, "Unable to clone the git repo ", ghsi.Channel.Spec.Pathname)
		ghsi.successful = false

		return err
	}

	klog.Info("Git commit: ", commitID)

	if strings.EqualFold(ghsi.reconcileRate, "medium") {
		// every 3 minutes, compare commit ID. If changed, reconcile resources.
		// every 15 minutes, reconcile resources without commit ID comparison.
		ghsi.count++

		if ghsi.commitID == "" {
			klog.Infof("No previous commit. DEPLOY")
		} else {
			if ghsi.count < 6 {
				if commitID == ghsi.commitID && ghsi.successful {
					klog.Infof("Appsub %s Git commit: %s hasn't changed. Skip reconcile.", hostkey.String(), commitID)

					return nil
				}
			} else {
				klog.Infof("Reconciling all resources")
				ghsi.count = 0
			}
		}
	}

	ghsi.commitID = commitID
	ghsi.successful = true

	return nil
}

func (ghsi *HubOfHubsSubscriberItem) cloneGitRepo() (commitID string, err error) {
	annotations := ghsi.Subscription.GetAnnotations()

	cloneDepth := 1

	if annotations[appv1.AnnotationGitCloneDepth] != "" {
		cloneDepth, err = strconv.Atoi(annotations[appv1.AnnotationGitCloneDepth])

		if err != nil {
			cloneDepth = 1

			klog.Error(err, " failed to convert git-clone-depth annotation to integer")
		}
	}

	ghsi.repoRoot = filepath.Join(sharedVolumePath, ghsi.Subscription.Name)

	cloneOptions := &utils.GitCloneOption{
		CommitHash:  ghsi.desiredCommit,
		RevisionTag: ghsi.desiredTag,
		CloneDepth:  cloneDepth,
		Branch:      utils.GetSubscriptionBranch(ghsi.Subscription),
		DestDir:     ghsi.repoRoot,
	}

	// Get the primary channel connection options
	primaryChannelConnectionConfig, err := getChannelConnectionConfig(ghsi.ChannelSecret, ghsi.ChannelConfigMap)

	if err != nil {
		return "", err
	}

	primaryChannelConnectionConfig.RepoURL = ghsi.Channel.Spec.Pathname
	primaryChannelConnectionConfig.InsecureSkipVerify = ghsi.Channel.Spec.InsecureSkipVerify
	cloneOptions.PrimaryConnectionOption = primaryChannelConnectionConfig

	// Get the secondary channel connection options
	if ghsi.SecondaryChannel != nil {
		// Get the secondary channel connection options
		secondaryChannelConnectionConfig, err := getChannelConnectionConfig(ghsi.SecondaryChannelSecret, ghsi.SecondaryChannelConfigMap)

		if err != nil {
			return "", err
		}

		secondaryChannelConnectionConfig.RepoURL = ghsi.SecondaryChannel.Spec.Pathname
		secondaryChannelConnectionConfig.InsecureSkipVerify = ghsi.SecondaryChannel.Spec.InsecureSkipVerify
		cloneOptions.SecondaryConnectionOption = secondaryChannelConnectionConfig
	}

	return utils.CloneGitRepo(cloneOptions)
}
