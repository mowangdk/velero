/*
Copyright the Velero contributors.

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

package restic

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"
)

const (
	alibabacloudCredentialsFileEnvVar = "ALIBABA_CLOUD_CREDENTIALS_FILE"

	alibabacloudMetadataURL                 = "http://100.100.100.200/latest/meta-data/"
	alibabacloudMetadataSecurityCredentials =  "ram/security-credentials"
)

// getAlibabaCloudResticEnvVars gets the environment variables that restic
// relies on (ALIBABACLOUD_PROFILE) base on info in the provided object 
// storage location config map
func getAlibabaCloudResticEnvVars(config map[string]string) (map[string]string, error) {
	result := make(map[string]string)

	if credentialsFile, ok := config[credentialsFileKey]; ok {
		result[alibabacloudCredentialsFileEnvVar] = credentialsFile
	}
	return result, nil
}

// RoleAuth define STS Token Response
type RoleAuth struct {
	AccessKeyID     string
	AccessKeySecret string
	Expiration      time.Time
	SecurityToken   string
	LastUpdated     time.Time
	Code            string
}

// getRamRole return ramrole name
func getRamRole() (string, error) {
	roleName, err := getMetaData(alibabacloudMetadataSecurityCredentials)
	if err != nil {
		return "", err
	}
	return roleName, nil
}

// getMetaData get metadata from ecs meta-server
func getMetaData(resource string) (string, error) {
	resp, err := http.Get(alibabacloudMetadataURL + resource)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// getSTSAK return AccessKeyID, AccessKeySecret and SecurityToken
func getSTSAK(ramrole string) (string, string, string, error) {
	// AliyunCSVeleroRole
	roleAuth := RoleAuth{}
	roleInfo, err := getMetaData(fmt.Sprintf(alibabacloudMetadataSecurityCredentials + "/%s", ramrole))
	if err != nil {
		return "", "", "", err
	}

	err = json.Unmarshal([]byte(roleInfo), &roleAuth)
	if err != nil {
		return "", "", "", err
	}
	return roleAuth.AccessKeyID, roleAuth.AccessKeySecret, roleAuth.SecurityToken, nil
}
