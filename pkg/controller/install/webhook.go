package install

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/operator-framework/api/pkg/operators/v1alpha1"
	hashutil "github.com/operator-framework/operator-lifecycle-manager/pkg/lib/kubernetes/pkg/util/hash"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/lib/ownerutil"
	log "github.com/sirupsen/logrus"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/rand"
)

func ValidWebhookRules(rules []admissionregistrationv1.RuleWithOperations) error {
	for _, rule := range rules {
		apiGroupMap := listToMap(rule.APIGroups)

		// protect OLM resources
		if contains(apiGroupMap, "*") {
			return fmt.Errorf("Webhook rules cannot include all groups")
		}

		if contains(apiGroupMap, "operators.coreos.com") {
			return fmt.Errorf("Webhook rules cannot include the OLM group")
		}

		// protect Admission Webhook resources
		if contains(apiGroupMap, "admissionregistration.k8s.io") {
			resourceGroupMap := listToMap(rule.Resources)
			if contains(resourceGroupMap, "*") || contains(resourceGroupMap, "MutatingWebhookConfiguration") || contains(resourceGroupMap, "ValidatingWebhookConfiguration") {
				return fmt.Errorf("Webhook rules cannot include MutatingWebhookConfiguration or ValidatingWebhookConfiguration resources")
			}
		}
	}
	return nil
}

func listToMap(list []string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, ele := range list {
		result[ele] = struct{}{}
	}
	return result
}

func contains(m map[string]struct{}, tar string) bool {
	_, present := m[tar]
	return present
}

func (i *StrategyDeploymentInstaller) createOrUpdateWebhook(caPEM []byte, desc v1alpha1.WebhookDescription) error {
	operatorGroups, err := i.strategyClient.GetOpLister().OperatorsV1().OperatorGroupLister().OperatorGroups(i.owner.GetNamespace()).List(labels.Everything())
	if err != nil || len(operatorGroups) != 1 {
		return fmt.Errorf("Error retrieving OperatorGroup info")
	}

	ogNamespacelabelSelector, err := operatorGroups[0].NamespaceLabelSelector()
	if err != nil {
		return err
	}

	switch desc.Type {
	case v1alpha1.ValidatingAdmissionWebhook:
		i.createOrUpdateValidatingWebhook(ogNamespacelabelSelector, caPEM, desc)
	case v1alpha1.MutatingAdmissionWebhook:
		i.createOrUpdateMutatingWebhook(ogNamespacelabelSelector, caPEM, desc)
	}
	return nil
}

func (i *StrategyDeploymentInstaller) createOrUpdateMutatingWebhook(ogNamespacelabelSelector *metav1.LabelSelector, caPEM []byte, desc v1alpha1.WebhookDescription) error {
	webhookLabels := ownerutil.OwnerLabel(i.owner, i.owner.GetObjectKind().GroupVersionKind().Kind)
	webhookLabels[WebhookDescKey] = desc.GenerateName
	webhookSelector := labels.SelectorFromSet(webhookLabels).String()
	existingWebhooks, err := i.strategyClient.GetOpClient().KubernetesInterface().AdmissionregistrationV1().MutatingWebhookConfigurations().List(context.TODO(), metav1.ListOptions{LabelSelector: webhookSelector})
	if err != nil {
		return err
	}

	if len(existingWebhooks.Items) == 0 {
		// Create a MutatingWebhookConfiguration
		webhook := admissionregistrationv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: desc.GenerateName + "-",
				Namespace:    i.owner.GetNamespace(),
				Labels:       ownerutil.OwnerLabel(i.owner, i.owner.GetObjectKind().GroupVersionKind().Kind),
			},
			Webhooks: []admissionregistrationv1.MutatingWebhook{
				desc.GetMutatingWebhook(i.owner.GetNamespace(), ogNamespacelabelSelector, caPEM),
			},
		}
		addWebhookLabels(&webhook, desc)
		if _, err := i.strategyClient.GetOpClient().KubernetesInterface().AdmissionregistrationV1().MutatingWebhookConfigurations().Create(context.TODO(), &webhook, metav1.CreateOptions{}); err != nil {
			log.Errorf("Webhooks: Error creating MutatingWebhookConfiguration: %v", err)
			return err
		}

		createOrUpdateConversionCrdInMutatingWebhook(desc, webhook, i)

		return nil
	}
	for _, webhook := range existingWebhooks.Items {
		// Update the list of webhooks
		webhook.Webhooks = []admissionregistrationv1.MutatingWebhook{
			desc.GetMutatingWebhook(i.owner.GetNamespace(), ogNamespacelabelSelector, caPEM),
		}
		addWebhookLabels(&webhook, desc)

		// Attempt an update
		if _, err := i.strategyClient.GetOpClient().KubernetesInterface().AdmissionregistrationV1().MutatingWebhookConfigurations().Update(context.TODO(), &webhook, metav1.UpdateOptions{}); err != nil {
			log.Warnf("could not update MutatingWebhookConfiguration %s", webhook.GetName())
			return err
		}

		createOrUpdateConversionCrdInMutatingWebhook(desc, webhook, i)
	}

	return nil
}

func createOrUpdateConversionCrdInMutatingWebhook(desc v1alpha1.WebhookDescription, webhook admissionregistrationv1.MutatingWebhookConfiguration, i *StrategyDeploymentInstaller) {
	// check if webhook has ConversionCrd field set, if true get crd of cluster and configure to use webhook effectively
	if desc.ConversionCrd != "" {
		crd, err := i.strategyClient.GetOpLister().APIExtensionsV1().CustomResourceDefinitionLister().Get(desc.ConversionCrd)
		if err != nil {
			log.Info("Crd not found %s, error: %s", desc.ConversionCrd, err.Error())
		}
		ctx := context.TODO()

		log.Info("Found conversionCrd %s", desc.ConversionCrd)
		path := "/convert"
		crd.Spec.Conversion.Strategy = "Webhook"
		crd.Spec.Conversion.Webhook.ClientConfig.CABundle = webhook.Webhooks[0].ClientConfig.CABundle
		crd.Spec.Conversion.Webhook.ClientConfig.Service.Name = webhook.Webhooks[0].ClientConfig.Service.Name
		crd.Spec.Conversion.Webhook.ClientConfig.Service.Namespace = webhook.Webhooks[0].ClientConfig.Service.Namespace
		crd.Spec.Conversion.Webhook.ClientConfig.Service.Path = &path
		crd.Spec.PreserveUnknownFields = false

		if _, err := i.strategyClient.GetOpClient().ApiextensionsInterface().ApiextensionsV1().CustomResourceDefinitions().Update(ctx, crd, metav1.UpdateOptions{}); err != nil {
			log.Info("Crd %s could not be updated, error: %s", desc.ConversionCrd, err.Error())
		}
	} else {
		log.Info("conversionCrd not found")
	}
}

func createOrUpdateConversionCrdInValidatingWebhook(desc v1alpha1.WebhookDescription, webhook admissionregistrationv1.ValidatingWebhookConfiguration, i *StrategyDeploymentInstaller) {
	// check if webhook has ConversionCrd field set, if true get crd of cluster and configure to use webhook effectively
	if desc.ConversionCrd != "" {
		crd, err := i.strategyClient.GetOpLister().APIExtensionsV1().CustomResourceDefinitionLister().Get(desc.ConversionCrd)
		if err != nil {
			log.Info("Crd not found %s, error: %s", desc.ConversionCrd, err.Error())
		}

		log.Info("Found conversionCrd %s", desc.ConversionCrd)

		path := "/convert"

		crd = &apiextensionsv1.CustomResourceDefinition{
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Conversion: &apiextensionsv1.CustomResourceConversion{
					Strategy: "Webhook",
					Webhook: &apiextensionsv1.WebhookConversion{
						ClientConfig: &apiextensionsv1.WebhookClientConfig{
							Service: &apiextensionsv1.ServiceReference{
								Namespace: webhook.Webhooks[0].ClientConfig.Service.Namespace,
								Name:      webhook.Webhooks[0].ClientConfig.Service.Name,
								Path:      &path,
								Port:      webhook.Webhooks[0].ClientConfig.Service.Port,
							},
							CABundle: webhook.Webhooks[0].ClientConfig.CABundle,
						},
					},
				},
				PreserveUnknownFields: false,
			},
		}
		if _, err = i.strategyClient.GetOpClient().ApiextensionsInterface().ApiextensionsV1().CustomResourceDefinitions().Update(context.TODO(), crd, metav1.UpdateOptions{}); err != nil {
			log.Info("Crd %s could not be updated, error: %s", desc.ConversionCrd, err.Error())
		}
	} else {
		log.Info("conversionCrd not found")
	}
}

func (i *StrategyDeploymentInstaller) createOrUpdateValidatingWebhook(ogNamespacelabelSelector *metav1.LabelSelector, caPEM []byte, desc v1alpha1.WebhookDescription) error {
	webhookLabels := ownerutil.OwnerLabel(i.owner, i.owner.GetObjectKind().GroupVersionKind().Kind)
	webhookLabels[WebhookDescKey] = desc.GenerateName
	webhookSelector := labels.SelectorFromSet(webhookLabels).String()

	existingWebhooks, err := i.strategyClient.GetOpClient().KubernetesInterface().AdmissionregistrationV1().ValidatingWebhookConfigurations().List(context.TODO(), metav1.ListOptions{LabelSelector: webhookSelector})
	if err != nil {
		return err
	}

	if len(existingWebhooks.Items) == 0 {
		// Create a ValidatingWebhookConfiguration
		webhook := admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: desc.GenerateName + "-",
				Namespace:    i.owner.GetNamespace(),
				Labels:       ownerutil.OwnerLabel(i.owner, i.owner.GetObjectKind().GroupVersionKind().Kind),
			},
			Webhooks: []admissionregistrationv1.ValidatingWebhook{
				desc.GetValidatingWebhook(i.owner.GetNamespace(), ogNamespacelabelSelector, caPEM),
			},
		}
		addWebhookLabels(&webhook, desc)

		if _, err := i.strategyClient.GetOpClient().KubernetesInterface().AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(context.TODO(), &webhook, metav1.CreateOptions{}); err != nil {
			log.Errorf("Webhooks: Error creating ValidatingWebhookConfiguration: %v", err)
			return err
		}

		createOrUpdateConversionCrdInValidatingWebhook(desc, webhook, i)

		return nil
	}
	for _, webhook := range existingWebhooks.Items {
		// Update the list of webhooks
		webhook.Webhooks = []admissionregistrationv1.ValidatingWebhook{
			desc.GetValidatingWebhook(i.owner.GetNamespace(), ogNamespacelabelSelector, caPEM),
		}
		addWebhookLabels(&webhook, desc)

		createOrUpdateConversionCrdInValidatingWebhook(desc, webhook, i)

		// Attempt an update
		if _, err := i.strategyClient.GetOpClient().KubernetesInterface().AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(context.TODO(), &webhook, metav1.UpdateOptions{}); err != nil {
			log.Warnf("could not update ValidatingWebhookConfiguration %s", webhook.GetName())
			return err
		}
	}

	return nil
}

const WebhookDescKey = "olm.webhook-description-generate-name"
const WebhookHashKey = "olm.webhook-description-hash"

// addWebhookLabels adds webhook labels to an object
func addWebhookLabels(object metav1.Object, webhookDesc v1alpha1.WebhookDescription) error {
	labels := object.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[WebhookDescKey] = webhookDesc.GenerateName
	labels[WebhookHashKey] = HashWebhookDesc(webhookDesc)
	object.SetLabels(labels)

	return nil
}

// HashWebhookDesc calculates a hash given a webhookDescription
func HashWebhookDesc(webhookDesc v1alpha1.WebhookDescription) string {
	hasher := fnv.New32a()
	hashutil.DeepHashObject(hasher, &webhookDesc)
	return rand.SafeEncodeString(fmt.Sprint(hasher.Sum32()))
}
