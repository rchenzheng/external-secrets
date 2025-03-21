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
package ibm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/IBM/go-sdk-core/v5/core"
	sm "github.com/IBM/secrets-manager-go-sdk/secretsmanagerv1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"

	esv1beta1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	"github.com/external-secrets/external-secrets/pkg/utils"
)

const (
	SecretsManagerEndpointEnv = "IBM_SECRETSMANAGER_ENDPOINT"
	STSEndpointEnv            = "IBM_STS_ENDPOINT"
	SSMEndpointEnv            = "IBM_SSM_ENDPOINT"

	errIBMClient                             = "cannot setup new ibm client: %w"
	errIBMCredSecretName                     = "invalid IBM SecretStore resource: missing IBM APIKey"
	errUninitalizedIBMProvider               = "provider IBM is not initialized"
	errInvalidClusterStoreMissingSKNamespace = "invalid ClusterStore, missing namespace"
	errFetchSAKSecret                        = "could not fetch SecretAccessKey secret: %w"
	errMissingSAK                            = "missing SecretAccessKey"
	errJSONSecretUnmarshal                   = "unable to unmarshal secret: %w"
)

type SecretManagerClient interface {
	GetSecret(getSecretOptions *sm.GetSecretOptions) (result *sm.GetSecret, response *core.DetailedResponse, err error)
}

type providerIBM struct {
	IBMClient SecretManagerClient
}

type client struct {
	kube        kclient.Client
	store       *esv1beta1.IBMProvider
	namespace   string
	storeKind   string
	credentials []byte
}

func (c *client) setAuth(ctx context.Context) error {
	credentialsSecret := &corev1.Secret{}
	credentialsSecretName := c.store.Auth.SecretRef.SecretAPIKey.Name
	if credentialsSecretName == "" {
		return fmt.Errorf(errIBMCredSecretName)
	}
	objectKey := types.NamespacedName{
		Name:      credentialsSecretName,
		Namespace: c.namespace,
	}

	// only ClusterStore is allowed to set namespace (and then it's required)
	if c.storeKind == esv1beta1.ClusterSecretStoreKind {
		if c.store.Auth.SecretRef.SecretAPIKey.Namespace == nil {
			return fmt.Errorf(errInvalidClusterStoreMissingSKNamespace)
		}
		objectKey.Namespace = *c.store.Auth.SecretRef.SecretAPIKey.Namespace
	}

	err := c.kube.Get(ctx, objectKey, credentialsSecret)
	if err != nil {
		return fmt.Errorf(errFetchSAKSecret, err)
	}

	c.credentials = credentialsSecret.Data[c.store.Auth.SecretRef.SecretAPIKey.Key]
	if (c.credentials == nil) || (len(c.credentials) == 0) {
		return fmt.Errorf(errMissingSAK)
	}
	return nil
}

// Empty GetAllSecrets.
func (ibm *providerIBM) GetAllSecrets(ctx context.Context, ref esv1beta1.ExternalSecretFind) (map[string][]byte, error) {
	// TO be implemented
	return nil, fmt.Errorf("GetAllSecrets not implemented")
}

func (ibm *providerIBM) GetSecret(ctx context.Context, ref esv1beta1.ExternalSecretDataRemoteRef) ([]byte, error) {
	if utils.IsNil(ibm.IBMClient) {
		return nil, fmt.Errorf(errUninitalizedIBMProvider)
	}

	secretType := sm.GetSecretOptionsSecretTypeArbitraryConst
	secretName := ref.Key
	nameSplitted := strings.Split(secretName, "/")

	if len(nameSplitted) > 1 {
		secretType = nameSplitted[0]
		secretName = nameSplitted[1]
	}

	switch secretType {
	case sm.GetSecretOptionsSecretTypeArbitraryConst:

		return getArbitrarySecret(ibm, &secretName)

	case sm.CreateSecretOptionsSecretTypeUsernamePasswordConst:

		if ref.Property == "" {
			return nil, fmt.Errorf("remoteRef.property required for secret type username_password")
		}
		return getUsernamePasswordSecret(ibm, &secretName, ref)

	case sm.CreateSecretOptionsSecretTypeIamCredentialsConst:

		return getIamCredentialsSecret(ibm, &secretName)

	case sm.CreateSecretOptionsSecretTypeImportedCertConst:

		if ref.Property == "" {
			return nil, fmt.Errorf("remoteRef.property required for secret type imported_cert")
		}

		return getImportCertSecret(ibm, &secretName, ref)
	default:
		return nil, fmt.Errorf("unknown secret type %s", secretType)
	}
}

func getArbitrarySecret(ibm *providerIBM, secretName *string) ([]byte, error) {
	response, _, err := ibm.IBMClient.GetSecret(
		&sm.GetSecretOptions{
			SecretType: core.StringPtr(sm.GetSecretOptionsSecretTypeArbitraryConst),
			ID:         secretName,
		})
	if err != nil {
		return nil, err
	}

	secret := response.Resources[0].(*sm.SecretResource)
	secretData := secret.SecretData.(map[string]interface{})
	arbitrarySecretPayload := secretData["payload"].(string)
	return []byte(arbitrarySecretPayload), nil
}

func getImportCertSecret(ibm *providerIBM, secretName *string, ref esv1beta1.ExternalSecretDataRemoteRef) ([]byte, error) {
	response, _, err := ibm.IBMClient.GetSecret(
		&sm.GetSecretOptions{
			SecretType: core.StringPtr(sm.CreateSecretOptionsSecretTypeImportedCertConst),
			ID:         secretName,
		})
	if err != nil {
		return nil, err
	}

	secret := response.Resources[0].(*sm.SecretResource)
	secretData := secret.SecretData.(map[string]interface{})

	if val, ok := secretData[ref.Property]; ok {
		return []byte(val.(string)), nil
	}
	return nil, fmt.Errorf("key %s does not exist in secret %s", ref.Property, ref.Key)
}

func getIamCredentialsSecret(ibm *providerIBM, secretName *string) ([]byte, error) {
	response, _, err := ibm.IBMClient.GetSecret(
		&sm.GetSecretOptions{
			SecretType: core.StringPtr(sm.CreateSecretOptionsSecretTypeIamCredentialsConst),
			ID:         secretName,
		})
	if err != nil {
		return nil, err
	}

	secret := response.Resources[0].(*sm.SecretResource)
	secretData := *secret.APIKey

	return []byte(secretData), nil
}

func getUsernamePasswordSecret(ibm *providerIBM, secretName *string, ref esv1beta1.ExternalSecretDataRemoteRef) ([]byte, error) {
	response, _, err := ibm.IBMClient.GetSecret(
		&sm.GetSecretOptions{
			SecretType: core.StringPtr(sm.CreateSecretOptionsSecretTypeUsernamePasswordConst),
			ID:         secretName,
		})
	if err != nil {
		return nil, err
	}

	secret := response.Resources[0].(*sm.SecretResource)
	secretData := secret.SecretData.(map[string]interface{})

	if val, ok := secretData[ref.Property]; ok {
		return []byte(val.(string)), nil
	}
	return nil, fmt.Errorf("key %s does not exist in secret %s", ref.Property, ref.Key)
}

func (ibm *providerIBM) GetSecretMap(ctx context.Context, ref esv1beta1.ExternalSecretDataRemoteRef) (map[string][]byte, error) {
	if utils.IsNil(ibm.IBMClient) {
		return nil, fmt.Errorf(errUninitalizedIBMProvider)
	}

	secretType := sm.GetSecretOptionsSecretTypeArbitraryConst
	secretName := ref.Key
	nameSplitted := strings.Split(secretName, "/")

	if len(nameSplitted) > 1 {
		secretType = nameSplitted[0]
		secretName = nameSplitted[1]
	}

	switch secretType {
	case sm.GetSecretOptionsSecretTypeArbitraryConst:
		response, _, err := ibm.IBMClient.GetSecret(
			&sm.GetSecretOptions{
				SecretType: core.StringPtr(sm.GetSecretOptionsSecretTypeArbitraryConst),
				ID:         &ref.Key,
			})
		if err != nil {
			return nil, err
		}

		secret := response.Resources[0].(*sm.SecretResource)
		secretData := secret.SecretData.(map[string]interface{})
		arbitrarySecretPayload := secretData["payload"].(string)

		kv := make(map[string]interface{})
		err = json.Unmarshal([]byte(arbitrarySecretPayload), &kv)
		if err != nil {
			return nil, fmt.Errorf(errJSONSecretUnmarshal, err)
		}

		secretMap := byteArrayMap(kv)

		return secretMap, nil

	case sm.CreateSecretOptionsSecretTypeUsernamePasswordConst:
		response, _, err := ibm.IBMClient.GetSecret(
			&sm.GetSecretOptions{
				SecretType: core.StringPtr(sm.CreateSecretOptionsSecretTypeUsernamePasswordConst),
				ID:         &secretName,
			})
		if err != nil {
			return nil, err
		}

		secret := response.Resources[0].(*sm.SecretResource)
		secretData := secret.SecretData.(map[string]interface{})

		secretMap := byteArrayMap(secretData)

		return secretMap, nil

	case sm.CreateSecretOptionsSecretTypeIamCredentialsConst:
		response, _, err := ibm.IBMClient.GetSecret(
			&sm.GetSecretOptions{
				SecretType: core.StringPtr(sm.CreateSecretOptionsSecretTypeIamCredentialsConst),
				ID:         &secretName,
			})
		if err != nil {
			return nil, err
		}

		secret := response.Resources[0].(*sm.SecretResource)
		secretData := *secret.APIKey

		secretMap := make(map[string][]byte)
		secretMap["apikey"] = []byte(secretData)

		return secretMap, nil

	case sm.CreateSecretOptionsSecretTypeImportedCertConst:
		response, _, err := ibm.IBMClient.GetSecret(
			&sm.GetSecretOptions{
				SecretType: core.StringPtr(sm.CreateSecretOptionsSecretTypeImportedCertConst),
				ID:         &secretName,
			})
		if err != nil {
			return nil, err
		}

		secret := response.Resources[0].(*sm.SecretResource)
		secretData := secret.SecretData.(map[string]interface{})

		secretMap := byteArrayMap(secretData)

		return secretMap, nil

	default:
		return nil, fmt.Errorf("unknown secret type %s", secretType)
	}
}

func byteArrayMap(secretData map[string]interface{}) map[string][]byte {
	secretMap := make(map[string][]byte)
	for k, v := range secretData {
		secretMap[k] = []byte(v.(string))
	}
	return secretMap
}

func (ibm *providerIBM) Close(ctx context.Context) error {
	return nil
}

func (ibm *providerIBM) Validate() error {
	return nil
}

func (ibm *providerIBM) ValidateStore(store esv1beta1.GenericStore) error {
	return nil
}

func (ibm *providerIBM) NewClient(ctx context.Context, store esv1beta1.GenericStore, kube kclient.Client, namespace string) (esv1beta1.SecretsClient, error) {
	storeSpec := store.GetSpec()
	ibmSpec := storeSpec.Provider.IBM

	iStore := &client{
		kube:      kube,
		store:     ibmSpec,
		namespace: namespace,
		storeKind: store.GetObjectKind().GroupVersionKind().Kind,
	}

	if err := iStore.setAuth(ctx); err != nil {
		return nil, err
	}

	secretsManager, err := sm.NewSecretsManagerV1(&sm.SecretsManagerV1Options{
		URL: *storeSpec.Provider.IBM.ServiceURL,
		Authenticator: &core.IamAuthenticator{
			ApiKey: string(iStore.credentials),
		},
	})

	// Setup retry options, but only if present
	if storeSpec.RetrySettings != nil {
		var retryAmount int
		var retryDuration time.Duration

		if storeSpec.RetrySettings.MaxRetries != nil {
			retryAmount = int(*storeSpec.RetrySettings.MaxRetries)
		} else {
			retryAmount = 3
		}

		if storeSpec.RetrySettings.RetryInterval != nil {
			retryDuration, err = time.ParseDuration(*storeSpec.RetrySettings.RetryInterval)
		} else {
			retryDuration = 5 * time.Second
		}

		if err == nil {
			secretsManager.Service.EnableRetries(retryAmount, retryDuration)
		}
	}

	if err != nil {
		return nil, fmt.Errorf(errIBMClient, err)
	}

	ibm.IBMClient = secretsManager
	return ibm, nil
}

func init() {
	esv1beta1.Register(&providerIBM{}, &esv1beta1.SecretStoreProvider{
		IBM: &esv1beta1.IBMProvider{},
	})
}
