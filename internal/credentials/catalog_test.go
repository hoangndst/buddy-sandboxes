package credentials

import "testing"

func TestBuiltInCredentialCatalogIsClosedAndRoutesByKind(t *testing.T) {
	services := RegisteredServices()
	wantIDs := []string{ServiceAnthropic, ServiceGitHub, ServiceOpenAI}
	if len(services) != len(wantIDs) {
		t.Fatalf("registered service count = %d, want %d", len(services), len(wantIDs))
	}
	for i, want := range wantIDs {
		if services[i].ID != want {
			t.Fatalf("service[%d] = %q, want %q", i, services[i].ID, want)
		}
	}
	if !IsProviderService(ServiceOpenAI) || !IsProviderService(ServiceAnthropic) || IsProviderService(ServiceGitHub) {
		t.Fatal("provider catalog classification is incorrect")
	}
	if !IsGitRepositoryService(ServiceGitHub) || IsGitRepositoryService(ServiceOpenAI) || IsGitRepositoryService(ServiceAnthropic) {
		t.Fatal("Git catalog classification is incorrect")
	}
	if _, ok := LookupService("unregistered"); ok {
		t.Fatal("unregistered service was accepted")
	}
}
