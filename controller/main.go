package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var labelPolicyGVR = schema.GroupVersionResource{Group: "field.vmware.com", Version: "v1", Resource: "labelpolicies"}

const fieldManager = "labelpolicy-controller"

type StringSlice []string

func (s *StringSlice) String() string {
	return fmt.Sprintf("%v", *s)
}

func (s *StringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type Selector struct {
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
}

type Target struct {
	Group    string    `json:"group"`
	Version  string    `json:"version"`
	Resource string    `json:"resource"`
	Kind     string    `json:"kind"`
	Name     string    `json:"name,omitempty"`
	Selector *Selector `json:"selector,omitempty"`
}

func (t Target) gvr() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: t.Group, Version: t.Version, Resource: t.Resource}
}

// key identifies a target by the same group/version/resource triple RBAC is scoped to -
// kind is deliberately excluded since it grants no additional access on its own.
func (t Target) key() string {
	return fmt.Sprintf("%s/%s/%s", t.Group, t.Version, t.Resource)
}

func (t Target) apiVersion() string {
	if t.Group == "" {
		return t.Version
	}
	return t.Group + "/" + t.Version
}

type LabelPolicySpec struct {
	Target *Target           `json:"target"`
	Labels map[string]string `json:"labels"`
}

type LabelPolicy struct {
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              *LabelPolicySpec `json:"spec,omitempty"`
}

func convertObj(obj any) (LabelPolicy, error) {
	policy := LabelPolicy{}
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return policy, fmt.Errorf("unable to convert to unstructured object")
	}
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.Object, &policy)
	if err != nil {
		return policy, err
	}
	return policy, nil
}

func specLabels(policy LabelPolicy) map[string]string {
	if policy.Spec == nil {
		return nil
	}
	return policy.Spec.Labels
}

func toInterfaceMap(m map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func matchingTargetNames(client *dynamic.DynamicClient, namespace string, target Target) ([]string, error) {
	if target.Name != "" {
		return []string{target.Name}, nil
	}
	if target.Selector == nil || len(target.Selector.MatchLabels) == 0 {
		return nil, fmt.Errorf("target has neither name nor selector.matchLabels set")
	}

	list, err := client.Resource(target.gvr()).Namespace(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(target.Selector.MatchLabels).String(),
	})
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.GetName())
	}
	return names, nil
}

// reconcile applies wantLabels to every object matched by policy.Spec.Target, via server-side
// apply scoped to metadata.labels under fieldManager. Passing an empty wantLabels (used on
// delete) retracts the keys this controller previously owned instead of deleting the object.
func reconcile(client *dynamic.DynamicClient, policy LabelPolicy, allowed map[string]bool, wantLabels map[string]interface{}) {
	if policy.Spec == nil || policy.Spec.Target == nil {
		log.Printf("labelpolicy %s/%s has no target, skipping", policy.Namespace, policy.Name)
		return
	}

	target := *policy.Spec.Target
	namespace := policy.Namespace

	if !allowed[target.key()] {
		log.Printf("labelpolicy %s/%s targets %s which is not in the allowed-target list, skipping", namespace, policy.Name, target.key())
		return
	}

	names, err := matchingTargetNames(client, namespace, target)
	if err != nil {
		log.Printf("unable to resolve targets for labelpolicy %s/%s: %v", namespace, policy.Name, err)
		return
	}

	for _, name := range names {
		patch := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": target.apiVersion(),
				"kind":       target.Kind,
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
					"labels":    wantLabels,
				},
			},
		}

		_, err := client.Resource(target.gvr()).Namespace(namespace).Apply(context.TODO(), name, patch, metav1.ApplyOptions{FieldManager: fieldManager, Force: true})
		if err != nil {
			log.Printf("unable to apply labels to %s %s/%s: %v", target.Kind, namespace, name, err)
			continue
		}
		log.Printf("successfully applied labels to %s %s/%s", target.Kind, namespace, name)
	}
}

func parseAllowedTargets(raw StringSlice) map[string]bool {
	allowed := map[string]bool{}
	for _, t := range raw {
		parts := strings.SplitN(t, "/", 4)
		if len(parts) != 4 {
			log.Fatalf("invalid --allowed-target %q, expected group/version/resource/kind", t)
		}
		allowed[fmt.Sprintf("%s/%s/%s", parts[0], parts[1], parts[2])] = true
	}
	return allowed
}

func main() {
	var allowedTargets StringSlice
	flag.Var(&allowedTargets, "allowed-target", "group/version/resource/kind this controller is permitted to label (can be specified multiple times)")
	resync := flag.Int("resync-period", 60, "time in seconds")

	flag.Parse()

	allowed := parseAllowedTargets(allowedTargets)

	var kubeconfig string
	if home := homedir.HomeDir(); home != "" {
		fmt.Println("using local kubeconfig")
		kubeconfig = filepath.Join(home, ".kube", "config")
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fmt.Println("Falling back to in-cluster config")
		config, err = rest.InClusterConfig()
		if err != nil {
			panic(err.Error())
		}
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return dynClient.Resource(labelPolicyGVR).Namespace("").List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return dynClient.Resource(labelPolicyGVR).Namespace("").Watch(context.TODO(), options)
			},
		},
		&unstructured.Unstructured{},
		time.Duration(*resync)*time.Second,
		cache.Indexers{},
	)

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			fmt.Println("Add event detected:", obj)
			policy, err := convertObj(obj)
			if err != nil {
				log.Printf("unable to convert object to structured labelpolicy: %v", err)
				return
			}
			reconcile(dynClient, policy, allowed, toInterfaceMap(specLabels(policy)))
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			fmt.Println("Update event detected:", newObj)
			policy, err := convertObj(newObj)
			if err != nil {
				log.Printf("unable to convert object to structured labelpolicy: %v", err)
				return
			}
			reconcile(dynClient, policy, allowed, toInterfaceMap(specLabels(policy)))
		},
		DeleteFunc: func(obj interface{}) {
			fmt.Println("Delete event detected:", obj)
			policy, err := convertObj(obj)
			if err != nil {
				log.Printf("unable to convert object to structured labelpolicy: %v", err)
				return
			}
			reconcile(dynClient, policy, allowed, map[string]interface{}{})
		},
	})

	stop := make(chan struct{})
	defer close(stop)

	go informer.Run(stop)

	if !cache.WaitForCacheSync(stop, informer.HasSynced) {
		panic("Timeout waiting for cache sync")
	}

	fmt.Println("Label Policy Controller started successfully")

	<-stop
}
