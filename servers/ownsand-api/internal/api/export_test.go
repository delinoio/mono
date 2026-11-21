// SPDX-License-Identifier: FSL-1.1-ALv2

package api

// WatcherCount reports the number of live sandbox change registrations. It is
// exported for the external api_test package, which cannot reach the unexported
// watch registry and which testenv (importing api) prevents an internal test
// from using.
func (s *Service) WatcherCount() int { return s.watch.count() }
