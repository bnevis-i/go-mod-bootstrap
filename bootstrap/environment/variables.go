/*******************************************************************************
 * Copyright 2019 Dell Inc.
 * Copyright 2020 Intel Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software distributed under the License
 * is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
 * or implied. See the License for the specific language governing permissions and limitations under
 * the License.
 *******************************************************************************/

package environment

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"unicode"

	"github.com/edgexfoundry/go-mod-configuration/pkg/types"
	"github.com/edgexfoundry/go-mod-core-contracts/clients/logger"
	"github.com/pelletier/go-toml"

	"github.com/edgexfoundry/go-mod-bootstrap/bootstrap/logging"
)

const (
	bootTimeoutSecondsDefault = 60
	bootRetrySecondsDefault   = 1
	defaultConfDirValue       = "./res"

	envKeyConfigUrl         = "EDGEX_CONFIGURATION_PROVIDER"
	envKeyRegistryUrl       = "edgex_registry"   // TODO: Remove for release v2.0.0
	envV1KeyStartupDuration = "startup_duration" // TODO: Remove for release v2.0.0
	envKeyStartupDuration   = "EDGEX_STARTUP_DURATION"
	envV1KeyStartupInterval = "startup_interval" // TODO: Remove for release v2.0.0
	envKeyStartupInterval   = "EDGEX_STARTUP_INTERVAL"
	envConfDir              = "EDGEX_CONF_DIR"
	envV1Profile            = "edgex_profile" // TODO: Remove for release v2.0.0
	envProfile              = "EDGEX_PROFILE"
	envFile                 = "EDGEX_CONFIG_FILE"
)

// Variables is receiver that holds Variables variables and encapsulates toml.Tree-based configuration field
// overrides.  Assumes "_" embedded in Variables variable key separates substructs; e.g. foo_bar_baz might refer to
//
// 		type foo struct {
// 			bar struct {
//          	baz string
//  		}
//		}
type Variables struct {
	variables map[string]string
}

// NewEnvironment constructor reads/stores os.Environ() for use by Variables receiver methods.
func NewVariables() *Variables {
	osEnv := os.Environ()
	e := &Variables{
		variables: make(map[string]string, len(osEnv)),
	}
	for _, env := range osEnv {
		// Can not use Split() on '=' since the value may have an '=' in it, so changed to use Index()
		index := strings.Index(env, "=")
		if index == -1 {
			continue
		}
		key := env[:index]
		value := env[index+1:]
		e.variables[key] = value
	}

	return e
}

// UseRegistry returns whether the envKeyRegistryUrl key is set
// TODO: remove this func for release v2.0.0 when envKeyRegistryUrl is removed
func (e *Variables) UseRegistry() bool {
	_, ok := os.LookupEnv(envKeyRegistryUrl)
	return ok
}

// OverrideConfiguration method replaces values in the configuration for matching Variables variable keys.
// serviceConfig must be pointer to the service configuration.
func (e *Variables) OverrideConfiguration(lc logger.LoggingClient, serviceConfig interface{}) (int, error) {
	var overrideCount = 0

	contents, err := toml.Marshal(reflect.ValueOf(serviceConfig).Elem().Interface())
	if err != nil {
		return 0, err
	}

	configTree, err := toml.LoadBytes(contents)
	if err != nil {
		return 0, err
	}

	// The toml.Tree API keys() only return to top level keys, rather that paths.
	// It is also missing a GetPaths so have to spin our own
	paths := e.buildPaths(configTree.ToMap())
	// Now that we have all the paths in the config tree, we need to create a map that has the uppercase versions as
	// the map keys and the original versions as the map values so we can match against uppercase names but use the
	// originals to set values.
	pathMap := e.buildUppercasePathMap(paths)

	for envVar, envValue := range e.variables {
		envKey := strings.Replace(envVar, "_", ".", -1)
		key, found := e.getKeyForMatchedPath(pathMap, envKey)
		if !found {
			continue
		}

		oldValue := configTree.Get(key)

		newValue, err := e.convertToType(oldValue, envValue)
		if err != nil {
			return 0, fmt.Errorf("environment value override failed for %s=%s: %s", envVar, envValue, err.Error())
		}

		configTree.Set(key, newValue)
		overrideCount++
		logEnvironmentOverride(lc, key, envVar, envValue)
	}

	// Put the configuration back into the services configuration struct with the overridden values
	err = configTree.Unmarshal(serviceConfig)
	if err != nil {
		return 0, fmt.Errorf("could not marshal toml configTree to configuration: %s", err.Error())
	}

	return overrideCount, nil
}

// buildPaths create the path strings for all settings in the Config tree's key map
func (e *Variables) buildPaths(keyMap map[string]interface{}) []string {
	var paths []string

	for key, item := range keyMap {
		if reflect.TypeOf(item).Kind() != reflect.Map {
			paths = append(paths, key)
			continue
		}

		subMap := item.(map[string]interface{})

		subPaths := e.buildPaths(subMap)
		for _, path := range subPaths {
			paths = append(paths, fmt.Sprintf("%s.%s", key, path))
		}
	}

	return paths
}

// buildUppercasePathMap builds a map where the key is the uppercase version of the path
// and the value is original version of the path
func (e *Variables) buildUppercasePathMap(paths []string) map[string]string {
	ucMap := make(map[string]string)
	for _, path := range paths {
		ucMap[strings.ToUpper(path)] = path
	}

	return ucMap
}

// getKeyForMatchedPath searches for match of the environment variable name with the uppercase path (pathMap keys)
// If matched found to original path (pathMap values) is returned as the "key"
// For backward compatibility a case insensitive comparision is currently used.
// TODO: For release v2.0.0 Change this to NOT check that `envVarName` is uppercase and only compare against uppercase
//  so only uppercase environment variable names will match.
func (e *Variables) getKeyForMatchedPath(pathMap map[string]string, envVarName string) (string, bool) {
	for ucKey, lcKey := range pathMap {
		compareKey := lcKey
		if isAllUpperCase(envVarName) {
			compareKey = ucKey
		}

		if compareKey == envVarName {
			return lcKey, true
		}
	}

	return "", false
}

// OverrideConfigProviderInfo overrides the Configuration Provider ServiceConfig values
// from an Variables variable value (if it exists).
func (_ *Variables) OverrideConfigProviderInfo(
	lc logger.LoggingClient,
	configProviderInfo types.ServiceConfig) (types.ServiceConfig, error) {

	// This is for backwards compatibility with Fuji Device Services.
	// If --registry=<url> is used then we must use the <url> for the configuration provider.
	// TODO: for release v2.0.0 just use envKeyConfigUrl
	key, url := getEnvironmentValue(envKeyConfigUrl, envKeyRegistryUrl)
	if len(url) > 0 {
		logEnvironmentOverride(lc, "Configuration Provider Information", key, url)

		if err := configProviderInfo.PopulateFromUrl(url); err != nil {
			return types.ServiceConfig{}, err
		}
	}

	return configProviderInfo, nil
}

// TODO: Remove this func for release V2.0.0
// This is for backwards compatibility with Fuji Device Services.
// If --registry=<url> is used then we must use the <url> for the configuration provider.
// GetRegistryProviderInfoOverride get the overrides for Registry Provider Config values
// from an Variables variable value (if it exists).
func (_ *Variables) GetRegistryProviderInfoOverride(lc logger.LoggingClient) string {
	url := os.Getenv(envKeyRegistryUrl)
	if len(url) > 0 {
		logEnvironmentOverride(lc, "Registry Provider Information", envKeyRegistryUrl, url)
	}

	return url
}

// convertToType attempts to convert the string value to the specified type of the old value
func (_ *Variables) convertToType(oldValue interface{}, value string) (newValue interface{}, err error) {
	switch oldValue.(type) {
	case []string:
		newValue = parseCommaSeparatedSlice(value)
	case []interface{}:
		newValue = parseCommaSeparatedSlice(value)
	case string:
		newValue = value
	case bool:
		newValue, err = strconv.ParseBool(value)
	case int:
		newValue, err = strconv.ParseInt(value, 10, strconv.IntSize)
		newValue = int(newValue.(int64))
	case int8:
		newValue, err = strconv.ParseInt(value, 10, 8)
		newValue = int8(newValue.(int64))
	case int16:
		newValue, err = strconv.ParseInt(value, 10, 16)
		newValue = int16(newValue.(int64))
	case int32:
		newValue, err = strconv.ParseInt(value, 10, 32)
		newValue = int32(newValue.(int64))
	case int64:
		newValue, err = strconv.ParseInt(value, 10, 64)
	case uint:
		newValue, err = strconv.ParseUint(value, 10, strconv.IntSize)
		newValue = uint(newValue.(uint64))
	case uint8:
		newValue, err = strconv.ParseUint(value, 10, 8)
		newValue = uint8(newValue.(uint64))
	case uint16:
		newValue, err = strconv.ParseUint(value, 10, 16)
		newValue = uint16(newValue.(uint64))
	case uint32:
		newValue, err = strconv.ParseUint(value, 10, 32)
		newValue = uint32(newValue.(uint64))
	case uint64:
		newValue, err = strconv.ParseUint(value, 10, 64)
	case float32:
		newValue, err = strconv.ParseFloat(value, 32)
		newValue = float32(newValue.(float64))
	case float64:
		newValue, err = strconv.ParseFloat(value, 64)
	default:
		err = fmt.Errorf(
			"configuration type of '%s' is not supported for environment variable override",
			reflect.TypeOf(oldValue).String())
	}

	return newValue, err
}

// StartupInfo provides the startup timer values which are applied to the StartupTimer created at boot.
type StartupInfo struct {
	Duration int
	Interval int
}

// GetStartupInfo gets the Service StartupInfo values from an Variables variable value (if it exists)
// or uses the default values.
func GetStartupInfo(serviceKey string) StartupInfo {
	// Logger hasn't be created at the time this info is needed so have to create local client.
	lc := logging.FactoryToStdout(serviceKey)

	startup := StartupInfo{
		Duration: bootTimeoutSecondsDefault,
		Interval: bootRetrySecondsDefault,
	}

	// Get the startup timer configuration form environment, if provided.
	// Have to support old V1 lowercase version of key and new uppercase version of the key until release v2.0.0
	key, value := getEnvironmentValue(envKeyStartupDuration, envV1KeyStartupDuration)
	if len(value) > 0 {
		logEnvironmentOverride(lc, "Startup Duration", key, value)

		if n, err := strconv.ParseInt(value, 10, 0); err == nil && n > 0 {
			startup.Duration = int(n)
		}
	}

	// Get the startup timer interval, if provided.
	// Have to support old V1 lowercase version of key and new uppercase version of the key unitl release v2.0.0
	key, value = getEnvironmentValue(envKeyStartupInterval, envV1KeyStartupInterval)
	if len(value) > 0 {
		logEnvironmentOverride(lc, "Startup Interval", key, value)

		if n, err := strconv.ParseInt(value, 10, 0); err == nil && n > 0 {
			startup.Interval = int(n)
		}
	}

	return startup
}

// GetConfDir get the config directory value from an Variables variable value (if it exists)
// or uses passed in value or default if previous result in blank.
func GetConfDir(lc logger.LoggingClient, configDir string) string {
	envValue := os.Getenv(envConfDir)
	if len(envValue) > 0 {
		configDir = envValue
		logEnvironmentOverride(lc, "-c/-confdir", envFile, envValue)
	}

	if len(configDir) == 0 {
		configDir = defaultConfDirValue
	}

	return configDir
}

// GetProfileDir get the profile directory value from an Variables variable value (if it exists)
// or uses passed in value or default if previous result in blank.
func GetProfileDir(lc logger.LoggingClient, profileDir string) string {
	// TODO: For release v2.0.0 just use envProfile
	key, envValue := getEnvironmentValue(envProfile, envV1Profile)
	if len(envValue) > 0 {
		profileDir = envValue
		logEnvironmentOverride(lc, "-p/-profile", key, envValue)
	}

	if len(profileDir) > 0 {
		profileDir += "/"
	}

	return profileDir
}

// GetConfigFileName gets the configuration filename value from an Variables variable value (if it exists)
// or uses passed in value.
func GetConfigFileName(lc logger.LoggingClient, configFileName string) string {
	envValue := os.Getenv(envFile)
	if len(envValue) > 0 {
		configFileName = envValue
		logEnvironmentOverride(lc, "-f/-file", envFile, envValue)
	}

	return configFileName
}

// parseCommaSeparatedSlice converts comma separated list to a string slice
func parseCommaSeparatedSlice(value string) (values []interface{}) {
	// Assumption is environment variable value is comma separated
	// Whitespace can vary so must be trimmed out
	result := strings.Split(strings.TrimSpace(value), ",")
	for _, entry := range result {
		values = append(values, strings.TrimSpace(entry))
	}

	return values
}

// TODO: Remove for release v2.0.0
// getEnvironmentValue attempt to get value for new upper case key and if not found attempts
// to get value for old lower case key. Returns the key last attempted and value from last attempt
func getEnvironmentValue(newKey string, v1Key string) (string, string) {
	key := newKey
	value := os.Getenv(key)
	if len(value) == 0 {
		key = v1Key
		value = os.Getenv(key)
	}
	return key, value
}

// logEnvironmentOverride logs that an option or configuration has been override by an environment variable.
func logEnvironmentOverride(lc logger.LoggingClient, name string, key string, value string) {
	lc.Info(fmt.Sprintf("Variables override of '%s' by environment variable: %s=%s", name, key, value))
}

// isAllUpperCase checks the key to determine if it is all uppercase letters
func isAllUpperCase(key string) bool {
	for _, ch := range key {
		if unicode.IsLetter(ch) && !unicode.IsUpper(ch) {
			return false
		}
	}

	return true
}
