package application

import "context"

// OpenProfile reports a non-terminal task of profile. Business tasks do not carry a document
// profile until the apparel task profile is bound on the execution bundle (S7-04). Until then an
// apparel plugin has no in-flight task to block plugin.disable.
func (s Service) OpenProfile(context.Context, string) (bool, error) {
	return false, nil
}
