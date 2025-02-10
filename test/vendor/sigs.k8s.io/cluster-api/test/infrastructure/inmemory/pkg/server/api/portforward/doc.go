/*
Copyright 2023 The Kubernetes Authors.

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

/*
Package portforward implements support for implementing a fake port forward service in the api.

The implementation is heavily inspired from https://github.com/kubernetes-sigs/kwok/blob/main/pkg/kwok/server/portforward/httpstream.go
(kudos to the team there!)
*/
package portforward
