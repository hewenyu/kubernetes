//go:build !windows
// +build !windows

/*
Copyright 2017 The Kubernetes Authors.

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

package transformation

import (
	"bytes"
	"context"
	"crypto/aes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/cryptobyte"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/storage/value"
	aestransformer "k8s.io/apiserver/pkg/storage/value/encrypt/aes"
	mock "k8s.io/apiserver/pkg/storage/value/encrypt/envelope/testing/v1beta1"
	kmsapi "k8s.io/kms/apis/v1beta1"
)

const (
	dekKeySizeLen = 2
	kmsAPIVersion = "v1beta1"
)

type envelope struct {
	providerName string
	rawEnvelope  []byte
	plainTextDEK []byte
}

func (r envelope) prefix() string {
	return fmt.Sprintf("k8s:enc:kms:v1:%s:", r.providerName)
}

func (r envelope) prefixLen() int {
	return len(r.prefix())
}

func (r envelope) dekLen() int {
	// DEK's length is stored in the two bytes that follow the prefix.
	return int(binary.BigEndian.Uint16(r.rawEnvelope[r.prefixLen() : r.prefixLen()+dekKeySizeLen]))
}

func (r envelope) cipherTextDEK() []byte {
	return r.rawEnvelope[r.prefixLen()+dekKeySizeLen : r.prefixLen()+dekKeySizeLen+r.dekLen()]
}

func (r envelope) startOfPayload(providerName string) int {
	return r.prefixLen() + dekKeySizeLen + r.dekLen()
}

func (r envelope) cipherTextPayload() []byte {
	return r.rawEnvelope[r.startOfPayload(r.providerName):]
}

func (r envelope) plainTextPayload(secretETCDPath string) ([]byte, error) {
	block, err := aes.NewCipher(r.plainTextDEK)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize AES Cipher: %v", err)
	}
	// etcd path of the key is used as the authenticated context - need to pass it to decrypt
	ctx := context.Background()
	dataCtx := value.DefaultContext([]byte(secretETCDPath))
	aesgcmTransformer := aestransformer.NewGCMTransformer(block)
	plainSecret, _, err := aesgcmTransformer.TransformFromStorage(ctx, r.cipherTextPayload(), dataCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to transform from storage via AESGCM, err: %w", err)
	}

	return plainSecret, nil
}

// TestKMSProvider is an integration test between KubeAPI, ETCD and KMS Plugin
// Concretely, this test verifies the following integration contracts:
// 1. Raw records in ETCD that were processed by KMS Provider should be prefixed with k8s:enc:kms:v1:grpc-kms-provider-name:
// 2. Data Encryption Key (DEK) should be generated by envelopeTransformer and passed to KMS gRPC Plugin
// 3. KMS gRPC Plugin should encrypt the DEK with a Key Encryption Key (KEK) and pass it back to envelopeTransformer
// 4. The cipherTextPayload (ex. Secret) should be encrypted via AES CBC transform
// 5. Prefix-EncryptedDEK-EncryptedPayload structure should be deposited to ETCD
// 6. Direct AES GCM decryption of the cipherTextPayload written with AES CBC transform does not work
// 7. Existing AES CBC secrets should be un-enveloped on direct reads from Kube API Server
// 8. No-op updates to the secret should cause new AES GCM key to be used
// 9. Direct AES GCM decryption works after the new AES GCM key is used
func TestKMSProvider(t *testing.T) {
	encryptionConfig := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - secrets
    providers:
    - kms:
       name: kms-provider
       cachesize: 1000
       endpoint: unix:///@kms-provider.sock
`
	providerName := "kms-provider"
	pluginMock, err := mock.NewBase64Plugin("@kms-provider.sock")
	if err != nil {
		t.Fatalf("failed to create mock of KMS Plugin: %v", err)
	}

	go pluginMock.Start()
	if err := mock.WaitForBase64PluginToBeUp(pluginMock); err != nil {
		t.Fatalf("Failed start plugin, err: %v", err)
	}
	defer pluginMock.CleanUp()

	test, err := newTransformTest(t, encryptionConfig, false, "", false)
	if err != nil {
		t.Fatalf("failed to start KUBE API Server with encryptionConfig\n %s, error: %v", encryptionConfig, err)
	}
	defer test.cleanUp()

	test.secret, err = test.createSecret(testSecret, testNamespace)
	if err != nil {
		t.Fatalf("Failed to create test secret, error: %v", err)
	}

	// Since Data Encryption Key (DEK) is randomly generated (per encryption operation), we need to ask KMS Mock for it.
	plainTextDEK := pluginMock.LastEncryptRequest()

	secretETCDPath := test.getETCDPathForResource(test.storageConfig.Prefix, "", "secrets", test.secret.Name, test.secret.Namespace)
	rawEnvelope, err := test.getRawSecretFromETCD()
	if err != nil {
		t.Fatalf("failed to read %s from etcd: %v", secretETCDPath, err)
	}
	envelopeData := envelope{
		providerName: providerName,
		rawEnvelope:  rawEnvelope,
		plainTextDEK: plainTextDEK,
	}

	wantPrefix := "k8s:enc:kms:v1:kms-provider:"
	if !bytes.HasPrefix(rawEnvelope, []byte(wantPrefix)) {
		t.Fatalf("expected secret to be prefixed with %s, but got %s", wantPrefix, rawEnvelope)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	decryptResponse, err := pluginMock.Decrypt(ctx, &kmsapi.DecryptRequest{Version: kmsAPIVersion, Cipher: envelopeData.cipherTextDEK()})
	if err != nil {
		t.Fatalf("failed to decrypt DEK, %v", err)
	}
	dekPlainAsWouldBeSeenByETCD := decryptResponse.Plain

	if !bytes.Equal(plainTextDEK, dekPlainAsWouldBeSeenByETCD) {
		t.Fatalf("expected plainTextDEK %v to be passed to KMS Plugin, but got %s",
			plainTextDEK, dekPlainAsWouldBeSeenByETCD)
	}

	plainSecret, err := envelopeData.plainTextPayload(secretETCDPath)
	if err != nil {
		t.Fatalf("failed to transform from storage via AESCBC, err: %v", err)
	}

	if !strings.Contains(string(plainSecret), secretVal) {
		t.Fatalf("expected %q after decryption, but got %q", secretVal, string(plainSecret))
	}

	secretClient := test.restClient.CoreV1().Secrets(testNamespace)
	// Secrets should be un-enveloped on direct reads from Kube API Server.
	s, err := secretClient.Get(ctx, testSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get Secret from %s, err: %v", testNamespace, err)
	}
	if secretVal != string(s.Data[secretKey]) {
		t.Fatalf("expected %s from KubeAPI, but got %s", secretVal, string(s.Data[secretKey]))
	}

	// write data using AES CBC to simulate a downgrade
	oldSecretBytes, err := base64.StdEncoding.DecodeString(oldSecret)
	if err != nil {
		t.Fatalf("failed to base64 decode old secret, err: %v", err)
	}
	oldKeyBytes, err := base64.StdEncoding.DecodeString(oldAESCBCKey)
	if err != nil {
		t.Fatalf("failed to base64 decode old key, err: %v", err)
	}
	block, err := aes.NewCipher(oldKeyBytes)
	if err != nil {
		t.Fatalf("invalid key, err: %v", err)
	}

	oldEncryptedSecretBytes, err := aestransformer.NewCBCTransformer(block).TransformToStorage(ctx, oldSecretBytes, value.DefaultContext(secretETCDPath))
	if err != nil {
		t.Fatalf("failed to encrypt old secret, err: %v", err)
	}

	oldEncryptedSecretBuf := cryptobyte.NewBuilder(nil)
	oldEncryptedSecretBuf.AddBytes([]byte(wantPrefix))
	oldEncryptedSecretBuf.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
		b.AddBytes([]byte(oldAESCBCKey))
	})
	oldEncryptedSecretBuf.AddBytes(oldEncryptedSecretBytes)

	_, err = test.writeRawRecordToETCD(secretETCDPath, oldEncryptedSecretBuf.BytesOrPanic())
	if err != nil {
		t.Fatalf("failed to write old encrypted secret, err: %v", err)
	}

	// confirm that direct AES GCM decryption does not work
	failingRawEnvelope, err := test.getRawSecretFromETCD()
	if err != nil {
		t.Fatalf("failed to read %s from etcd: %v", secretETCDPath, err)
	}
	failingOldEnvelope := envelope{
		providerName: providerName,
		rawEnvelope:  failingRawEnvelope,
		plainTextDEK: oldKeyBytes,
	}
	failingOldPlainSecret, err := failingOldEnvelope.plainTextPayload(secretETCDPath)
	if err == nil {
		t.Fatalf("AESGCM decryption failure not seen, data: %s", string(failingOldPlainSecret))
	}

	// Existing AES CBC secrets should be un-enveloped on direct reads from Kube API Server.
	oldSecretObj, err := secretClient.Get(ctx, testSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to read old secret via Kube API, err: %v", err)
	}
	if oldSecretVal != string(oldSecretObj.Data[secretKey]) {
		t.Fatalf("expected %s from KubeAPI, but got %s", oldSecretVal, string(oldSecretObj.Data[secretKey]))
	}

	// no-op update should cause new AES GCM key to be used
	oldSecretUpdated, err := secretClient.Update(ctx, oldSecretObj, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("failed to update old secret via Kube API, err: %v", err)
	}
	if oldSecretObj.ResourceVersion == oldSecretUpdated.ResourceVersion {
		t.Fatalf("old secret not updated on no-op write: %s", oldSecretObj.ResourceVersion)
	}

	// confirm that direct AES GCM decryption works
	oldRawEnvelope, err := test.getRawSecretFromETCD()
	if err != nil {
		t.Fatalf("failed to read %s from etcd: %v", secretETCDPath, err)
	}
	oldEnvelope := envelope{
		providerName: providerName,
		rawEnvelope:  oldRawEnvelope,
		plainTextDEK: pluginMock.LastEncryptRequest(),
	}
	if !bytes.HasPrefix(oldRawEnvelope, []byte(wantPrefix)) {
		t.Fatalf("expected secret to be prefixed with %s, but got %s", wantPrefix, oldRawEnvelope)
	}
	oldPlainSecret, err := oldEnvelope.plainTextPayload(secretETCDPath)
	if err != nil {
		t.Fatalf("failed to transform from storage via AESGCM, err: %v", err)
	}
	if !strings.Contains(string(oldPlainSecret), oldSecretVal) {
		t.Fatalf("expected %q after decryption, but got %q", oldSecretVal, string(oldPlainSecret))
	}
}

// TestECHotReload is an integration test that verifies hot reload of KMS encryption config works.
// This test asserts following scenarios:
// 1. start at 'kms-provider'
// 2. create some secrets
// 3. add 'new-kms-provider' as write KMS (this is okay because we only have 1 API server)
// 4. wait for config to be observed
// 5. run storage migration on secrets
// 6. confirm that secrets have the new prefix
// 7. remove 'kms-provider'
// 8. wait for config to be observed
// 9. confirm that reads still work
// 10. confirm that cluster wide secret read still works
// 11. confirm that api server can restart with last applied encryption config
func TestEncryptionConfigHotReload(t *testing.T) {
	encryptionConfig := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - secrets
    providers:
    - kms:
       name: kms-provider
       cachesize: 1000
       endpoint: unix:///@kms-provider.sock
`
	pluginMock, err := mock.NewBase64Plugin("@kms-provider.sock")
	if err != nil {
		t.Fatalf("failed to create mock of KMS Plugin: %v", err)
	}

	go pluginMock.Start()
	if err := mock.WaitForBase64PluginToBeUp(pluginMock); err != nil {
		t.Fatalf("Failed start plugin, err: %v", err)
	}
	defer pluginMock.CleanUp()

	var restarted bool
	test, err := newTransformTest(t, encryptionConfig, true, "", false)
	if err != nil {
		t.Fatalf("failed to start KUBE API Server with encryptionConfig\n %s, error: %v", encryptionConfig, err)
	}
	defer func() {
		if !restarted {
			test.cleanUp()
		}
	}()

	test.secret, err = test.createSecret(testSecret, testNamespace)
	if err != nil {
		t.Fatalf("Failed to create test secret, error: %v", err)
	}

	// create a new secret in default namespace. This is to assert cluster wide read works after hot reload.
	_, err = test.createSecret(fmt.Sprintf("%s-%s", testSecret, "1"), "default")
	if err != nil {
		t.Fatalf("Failed to create test secret in default namespace, error: %v", err)
	}

	_, err = test.createConfigMap(testConfigmap, testNamespace)
	if err != nil {
		t.Fatalf("Failed to create test configmap, error: %v", err)
	}

	// test if hot reload controller is healthy
	mustBeHealthy(t, "/poststarthook/start-encryption-provider-config-automatic-reload", "ok", test.kubeAPIServer.ClientConfig)

	encryptionConfigWithNewProvider := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - secrets
    providers:
    - kms:
       name: new-kms-provider-for-secrets
       cachesize: 1000
       endpoint: unix:///@new-kms-provider.sock
    - kms:
       name: kms-provider
       cachesize: 1000
       endpoint: unix:///@kms-provider.sock
  - resources:
    - configmaps
    providers:
    - kms:
       name: new-kms-provider-for-configmaps
       cachesize: 1000
       endpoint: unix:///@new-kms-provider.sock
    - identity: {}
`
	// start new KMS Plugin
	newPluginMock, err := mock.NewBase64Plugin("@new-kms-provider.sock")
	if err != nil {
		t.Fatalf("failed to create mock of KMS Plugin: %v", err)
	}

	go newPluginMock.Start()
	if err := mock.WaitForBase64PluginToBeUp(newPluginMock); err != nil {
		t.Fatalf("Failed start plugin, err: %v", err)
	}
	defer newPluginMock.CleanUp()

	// update encryption config
	if err := os.WriteFile(path.Join(test.configDir, encryptionConfigFileName), []byte(encryptionConfigWithNewProvider), 0644); err != nil {
		t.Fatalf("failed to update encryption config, err: %v", err)
	}

	wantPrefixForSecrets := "k8s:enc:kms:v1:new-kms-provider-for-secrets:"

	// implementing this brute force approach instead of fancy channel notification to avoid test specific code in prod.
	// wait for config to be observed
	verifyIfKMSTransformersSwapped(t, wantPrefixForSecrets, test)

	// run storage migration
	// get secrets
	secretsList, err := test.restClient.CoreV1().Secrets("").List(
		context.TODO(),
		metav1.ListOptions{},
	)
	if err != nil {
		t.Fatalf("failed to list secrets, err: %v", err)
	}

	for _, secret := range secretsList.Items {
		// update secret
		_, err = test.restClient.CoreV1().Secrets(secret.Namespace).Update(
			context.TODO(),
			&secret,
			metav1.UpdateOptions{},
		)
		if err != nil {
			t.Fatalf("failed to update secret, err: %v", err)
		}
	}

	// get configmaps
	configmapsList, err := test.restClient.CoreV1().ConfigMaps("").List(
		context.TODO(),
		metav1.ListOptions{},
	)
	if err != nil {
		t.Fatalf("failed to list configmaps, err: %v", err)
	}

	for _, configmap := range configmapsList.Items {
		// update configmap
		_, err = test.restClient.CoreV1().ConfigMaps(configmap.Namespace).Update(
			context.TODO(),
			&configmap,
			metav1.UpdateOptions{},
		)
		if err != nil {
			t.Fatalf("failed to update configmap, err: %v", err)
		}
	}

	// assert that resources has new prefix
	secretETCDPath := test.getETCDPathForResource(test.storageConfig.Prefix, "", "secrets", test.secret.Name, test.secret.Namespace)
	rawEnvelope, err := test.getRawSecretFromETCD()
	if err != nil {
		t.Fatalf("failed to read %s from etcd: %v", secretETCDPath, err)
	}

	// assert secret
	if !bytes.HasPrefix(rawEnvelope, []byte(wantPrefixForSecrets)) {
		t.Fatalf("expected secret to be prefixed with %s, but got %s", wantPrefixForSecrets, rawEnvelope)
	}

	rawConfigmapEnvelope, err := test.readRawRecordFromETCD(test.getETCDPathForResource(test.storageConfig.Prefix, "", "configmaps", testConfigmap, testNamespace))

	if err != nil {
		t.Fatalf("failed to read %s from etcd: %v", test.getETCDPathForResource(test.storageConfig.Prefix, "", "configmaps", testConfigmap, testNamespace), err)
	}

	// assert prefix for configmap
	wantPrefixForConfigmaps := "k8s:enc:kms:v1:new-kms-provider-for-configmaps:"
	if !bytes.HasPrefix(rawConfigmapEnvelope.Kvs[0].Value, []byte(wantPrefixForConfigmaps)) {
		t.Fatalf("expected configmap to be prefixed with %s, but got %s", wantPrefixForConfigmaps, rawConfigmapEnvelope.Kvs[0].Value)
	}

	// remove old KMS provider
	encryptionConfigWithoutOldProvider := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - secrets
    providers:
    - kms:
       name: new-kms-provider-for-secrets
       cachesize: 1000
       endpoint: unix:///@new-kms-provider.sock
  - resources:
    - configmaps
    providers:
    - kms:
       name: new-kms-provider-for-configmaps
       cachesize: 1000
       endpoint: unix:///@new-kms-provider.sock
`

	// update encryption config and wait for hot reload
	if err := os.WriteFile(path.Join(test.configDir, encryptionConfigFileName), []byte(encryptionConfigWithoutOldProvider), 0644); err != nil {
		t.Fatalf("failed to update encryption config, err: %v", err)
	}

	// wait for config to be observed
	verifyIfKMSTransformersSwapped(t, wantPrefixForSecrets, test)

	// confirm that reading secrets still works
	_, err = test.restClient.CoreV1().Secrets(testNamespace).Get(
		context.TODO(),
		testSecret,
		metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("failed to read secret, err: %v", err)
	}

	// make sure cluster wide secrets read still works
	_, err = test.restClient.CoreV1().Secrets("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list secrets, err: %v", err)
	}

	// make sure cluster wide configmaps read still works
	_, err = test.restClient.CoreV1().ConfigMaps("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list configmaps, err: %v", err)
	}

	// restart kube-apiserver with last applied encryption config and assert that server can start
	previousConfigDir := test.configDir
	test.shutdownAPIServer()
	restarted = true
	test, err = newTransformTest(t, "", true, previousConfigDir, false)
	if err != nil {
		t.Fatalf("failed to start KUBE API Server with encryptionConfig\n %s, error: %v", encryptionConfig, err)
	}
	test.cleanUp()
}

func TestEncryptionConfigHotReloadFileWatch(t *testing.T) {
	testCases := []struct {
		fileUpdateMethod string
	}{
		{
			fileUpdateMethod: "truncate",
		},
		{
			fileUpdateMethod: "deleteAndCreate",
		},
		{
			fileUpdateMethod: "move",
		},
		{
			fileUpdateMethod: "symLink",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.fileUpdateMethod, func(t *testing.T) {
			encryptionConfig := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - secrets
    providers:
    - kms:
       name: kms-provider
       cachesize: 1000
       endpoint: unix:///@kms-provider.sock
`
			pluginMock, err := mock.NewBase64Plugin("@kms-provider.sock")
			if err != nil {
				t.Fatalf("failed to create mock of KMS Plugin: %v", err)
			}

			go pluginMock.Start()
			if err := mock.WaitForBase64PluginToBeUp(pluginMock); err != nil {
				t.Fatalf("Failed start plugin, err: %v", err)
			}
			defer pluginMock.CleanUp()

			var test *transformTest
			if tc.fileUpdateMethod == "symLink" {
				test, err = newTransformTest(t, encryptionConfig, true, "", true)
				if err != nil {
					t.Fatalf("failed to start KUBE API Server with encryptionConfig\n %s, error: %v", encryptionConfig, err)
				}
			} else {
				test, err = newTransformTest(t, encryptionConfig, true, "", false)
				if err != nil {
					t.Fatalf("failed to start KUBE API Server with encryptionConfig\n %s, error: %v", encryptionConfig, err)
				}
			}
			defer test.cleanUp()

			test.secret, err = test.createSecret(testSecret, testNamespace)
			if err != nil {
				t.Fatalf("Failed to create test secret, error: %v", err)
			}

			// test if hot reload controller is healthy
			mustBeHealthy(t, "/poststarthook/start-encryption-provider-config-automatic-reload", "ok", test.kubeAPIServer.ClientConfig)

			encryptionConfigWithNewProvider := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - secrets
    providers:
    - kms:
       name: new-kms-provider-for-secrets
       cachesize: 1000
       endpoint: unix:///@new-kms-provider.sock
    - kms:
       name: kms-provider
       cachesize: 1000
       endpoint: unix:///@kms-provider.sock
  - resources:
    - configmaps
    providers:
    - kms:
       name: new-kms-provider-for-configmaps
       cachesize: 1000
       endpoint: unix:///@new-kms-provider.sock
    - identity: {}
`
			// start new KMS Plugin
			newPluginMock, err := mock.NewBase64Plugin("@new-kms-provider.sock")
			if err != nil {
				t.Fatalf("failed to create mock of KMS Plugin: %v", err)
			}

			go newPluginMock.Start()
			if err := mock.WaitForBase64PluginToBeUp(newPluginMock); err != nil {
				t.Fatalf("Failed start plugin, err: %v", err)
			}
			defer newPluginMock.CleanUp()

			switch tc.fileUpdateMethod {
			case "truncate":
				// update encryption config
				// os.WriteFile truncates the file before writing
				if err := os.WriteFile(path.Join(test.configDir, encryptionConfigFileName), []byte(encryptionConfigWithNewProvider), 0644); err != nil {
					t.Fatalf("failed to update encryption config, err: %v", err)
				}
			case "deleteAndCreate":
				// update encryption config
				// os.Remove deletes the file before creating a new one
				if err := os.Remove(path.Join(test.configDir, encryptionConfigFileName)); err != nil {
					t.Fatalf("failed to remove encryption config, err: %v", err)
				}
				file, err := os.Create(path.Join(test.configDir, encryptionConfigFileName))
				if err != nil {
					t.Fatalf("failed to create encryption config, err: %v", err)
				}
				if _, err := file.Write([]byte(encryptionConfigWithNewProvider)); err != nil {
					t.Fatalf("failed to write encryption config, err: %v", err)
				}
				if err := file.Close(); err != nil {
					t.Fatalf("failed to close encryption config, err: %v", err)
				}
			case "move":
				// update encryption config
				// write new config to a temp file
				if err := os.WriteFile(path.Join(test.configDir, encryptionConfigFileName+".tmp"), []byte(encryptionConfigWithNewProvider), 0644); err != nil {
					t.Fatalf("failed to write config to tmp file, err: %v", err)
				}

				// move the temp file to the original file
				if err := os.Rename(path.Join(test.configDir, encryptionConfigFileName+".tmp"), path.Join(test.configDir, encryptionConfigFileName)); err != nil {
					t.Fatalf("failed to move encryption config, err: %v", err)
				}
			case "symLink":
				// update encryption config
				// write new config in a parent directory.
				if err := os.WriteFile(path.Join(test.configParentDir, encryptionConfigFileName), []byte(encryptionConfigWithNewProvider), 0644); err != nil {
					t.Fatalf("failed to update encryption config, err: %v", err)
				}
			default:
				t.Fatalf("unknown file update method: %s", tc.fileUpdateMethod)
			}

			wantPrefix := "k8s:enc:kms:v1:new-kms-provider-for-secrets:"

			// implementing this brute force approach instead of fancy channel notification to avoid test specific code in prod.
			// wait for config to be observed
			verifyIfKMSTransformersSwapped(t, wantPrefix, test)

			// run storage migration
			// get secrets
			secretsList, err := test.restClient.CoreV1().Secrets("").List(
				context.TODO(),
				metav1.ListOptions{},
			)
			if err != nil {
				t.Fatalf("failed to list secrets, err: %v", err)
			}

			for _, secret := range secretsList.Items {
				// update secret
				_, err = test.restClient.CoreV1().Secrets(secret.Namespace).Update(
					context.TODO(),
					&secret,
					metav1.UpdateOptions{},
				)
				if err != nil {
					t.Fatalf("failed to update secret, err: %v", err)
				}
			}

			// assert that resources has new prefix
			secretETCDPath := test.getETCDPathForResource(test.storageConfig.Prefix, "", "secrets", test.secret.Name, test.secret.Namespace)
			rawEnvelope, err := test.getRawSecretFromETCD()
			if err != nil {
				t.Fatalf("failed to read %s from etcd: %v", secretETCDPath, err)
			}

			// assert secret
			if !bytes.HasPrefix(rawEnvelope, []byte(wantPrefix)) {
				t.Fatalf("expected secret to be prefixed with %s, but got %s", wantPrefix, rawEnvelope)
			}
		})
	}
}

func verifyIfKMSTransformersSwapped(t *testing.T, wantPrefix string, test *transformTest) {
	t.Helper()

	var swapErr error
	// delete and recreate same secret flakes, so create a new secret with a different index until new prefix is observed
	// generate a random int to be used in secret name
	idx := rand.Intn(100000)

	pollErr := wait.PollImmediate(time.Second, wait.ForeverTestTimeout, func() (bool, error) {
		// create secret
		secretName := fmt.Sprintf("secret-%d", idx)
		_, err := test.createSecret(secretName, "default")
		if err != nil {
			t.Fatalf("Failed to create test secret, error: %v", err)
		}

		rawEnvelope, err := test.readRawRecordFromETCD(test.getETCDPathForResource(test.storageConfig.Prefix, "", "secrets", secretName, "default"))
		if err != nil {
			t.Fatalf("failed to read %s from etcd: %v", test.getETCDPathForResource(test.storageConfig.Prefix, "", "secrets", secretName, "default"), err)
		}

		// check prefix
		if !bytes.HasPrefix(rawEnvelope.Kvs[0].Value, []byte(wantPrefix)) {
			idx++

			swapErr = fmt.Errorf("expected secret to be prefixed with %s, but got %s", wantPrefix, rawEnvelope.Kvs[0].Value)

			// return nil error to continue polling till timeout
			return false, nil
		}

		return true, nil
	})
	if pollErr == wait.ErrWaitTimeout {
		t.Fatalf("failed to verify if kms transformers swapped, err: %v", swapErr)
	}
}

func TestKMSHealthz(t *testing.T) {
	encryptionConfig := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - secrets
    providers:
    - kms:
       name: provider-1
       endpoint: unix:///@kms-provider-1.sock
    - kms:
       name: provider-2
       endpoint: unix:///@kms-provider-2.sock
`

	pluginMock1, err := mock.NewBase64Plugin("@kms-provider-1.sock")
	if err != nil {
		t.Fatalf("failed to create mock of KMS Plugin #1: %v", err)
	}

	if err := pluginMock1.Start(); err != nil {
		t.Fatalf("Failed to start kms-plugin, err: %v", err)
	}
	defer pluginMock1.CleanUp()
	if err := mock.WaitForBase64PluginToBeUp(pluginMock1); err != nil {
		t.Fatalf("Failed to start plugin #1, err: %v", err)
	}

	pluginMock2, err := mock.NewBase64Plugin("@kms-provider-2.sock")
	if err != nil {
		t.Fatalf("Failed to create mock of KMS Plugin #2: err: %v", err)
	}
	if err := pluginMock2.Start(); err != nil {
		t.Fatalf("Failed to start kms-plugin, err: %v", err)
	}
	defer pluginMock2.CleanUp()
	if err := mock.WaitForBase64PluginToBeUp(pluginMock2); err != nil {
		t.Fatalf("Failed to start KMS Plugin #2: err: %v", err)
	}

	test, err := newTransformTest(t, encryptionConfig, false, "", false)
	if err != nil {
		t.Fatalf("Failed to start kube-apiserver, error: %v", err)
	}
	defer test.cleanUp()

	// Name of the healthz check is always "kms-provider-0" and it covers all kms plugins.

	// Stage 1 - Since all kms-plugins are guaranteed to be up, healthz checks for:
	// healthz/kms-provider-0 and /healthz/kms-provider-1 should be OK.
	mustBeHealthy(t, "/kms-provider-0", "ok", test.kubeAPIServer.ClientConfig)
	mustBeHealthy(t, "/kms-provider-1", "ok", test.kubeAPIServer.ClientConfig)

	// Stage 2 - kms-plugin for provider-1 is down. Therefore, expect the healthz check
	// to fail and report that provider-1 is down
	pluginMock1.EnterFailedState()
	mustBeUnHealthy(t, "/kms-provider-0",
		"internal server error: rpc error: code = FailedPrecondition desc = failed precondition - key disabled",
		test.kubeAPIServer.ClientConfig)
	mustBeHealthy(t, "/kms-provider-1", "ok", test.kubeAPIServer.ClientConfig)
	pluginMock1.ExitFailedState()

	// Stage 3 - kms-plugin for provider-1 is now up. Therefore, expect the health check for provider-1
	// to succeed now, but provider-2 is now down.
	pluginMock2.EnterFailedState()
	mustBeHealthy(t, "/kms-provider-0", "ok", test.kubeAPIServer.ClientConfig)
	mustBeUnHealthy(t, "/kms-provider-1",
		"internal server error: rpc error: code = FailedPrecondition desc = failed precondition - key disabled",
		test.kubeAPIServer.ClientConfig)
	pluginMock2.ExitFailedState()

	// Stage 4 - All kms-plugins are once again up,
	// the healthz check should be OK.
	mustBeHealthy(t, "/kms-provider-0", "ok", test.kubeAPIServer.ClientConfig)
	mustBeHealthy(t, "/kms-provider-1", "ok", test.kubeAPIServer.ClientConfig)
}

func TestKMSHealthzWithReload(t *testing.T) {
	encryptionConfig := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - secrets
    providers:
    - kms:
       name: provider-1
       endpoint: unix:///@kms-provider-1.sock
    - kms:
       name: provider-2
       endpoint: unix:///@kms-provider-2.sock
`

	pluginMock1, err := mock.NewBase64Plugin("@kms-provider-1.sock")
	if err != nil {
		t.Fatalf("failed to create mock of KMS Plugin #1: %v", err)
	}

	if err := pluginMock1.Start(); err != nil {
		t.Fatalf("Failed to start kms-plugin, err: %v", err)
	}
	defer pluginMock1.CleanUp()
	if err := mock.WaitForBase64PluginToBeUp(pluginMock1); err != nil {
		t.Fatalf("Failed to start plugin #1, err: %v", err)
	}

	pluginMock2, err := mock.NewBase64Plugin("@kms-provider-2.sock")
	if err != nil {
		t.Fatalf("Failed to create mock of KMS Plugin #2: err: %v", err)
	}
	if err := pluginMock2.Start(); err != nil {
		t.Fatalf("Failed to start kms-plugin, err: %v", err)
	}
	defer pluginMock2.CleanUp()
	if err := mock.WaitForBase64PluginToBeUp(pluginMock2); err != nil {
		t.Fatalf("Failed to start KMS Plugin #2: err: %v", err)
	}

	test, err := newTransformTest(t, encryptionConfig, true, "", false)
	if err != nil {
		t.Fatalf("Failed to start kube-apiserver, error: %v", err)
	}
	defer test.cleanUp()

	// Name of the healthz check is always "kms-provider-0" and it covers all kms plugins.

	// Stage 1 - Since all kms-plugins are guaranteed to be up,
	// the healthz check should be OK.
	mustBeHealthy(t, "/kms-providers", "ok", test.kubeAPIServer.ClientConfig)

	// Stage 2 - kms-plugin for provider-1 is down. Therefore, expect the healthz check
	// to fail and report that provider-1 is down
	pluginMock1.EnterFailedState()
	mustBeUnHealthy(t, "/kms-providers",
		"internal server error: kms-provider-0: failed to perform encrypt section of the healthz check for KMS Provider provider-1, error: rpc error: code = FailedPrecondition desc = failed precondition - key disabled",
		test.kubeAPIServer.ClientConfig)
	pluginMock1.ExitFailedState()

	// Stage 3 - kms-plugin for provider-1 is now up. Therefore, expect the health check for provider-1
	// to succeed now, but provider-2 is now down.
	pluginMock2.EnterFailedState()
	mustBeUnHealthy(t, "/kms-providers",
		"internal server error: kms-provider-1: failed to perform encrypt section of the healthz check for KMS Provider provider-2, error: rpc error: code = FailedPrecondition desc = failed precondition - key disabled",
		test.kubeAPIServer.ClientConfig)
	pluginMock2.ExitFailedState()

	// Stage 4 - All kms-plugins are once again up,
	// the healthz check should be OK.
	mustBeHealthy(t, "/kms-providers", "ok", test.kubeAPIServer.ClientConfig)
}
