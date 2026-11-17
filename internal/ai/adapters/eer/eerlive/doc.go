// Package eerlive configures live runs against a running EER from the environment (decision E-2:
// the TELOS platform on the owner's machine). Everything but this file is built only with -tags
// eerlive (make eer-live), never under CI=true; without the tag the package is empty.
//
// Variables (no defaults for where EER is or who Brant is there; credentials only from the
// environment, never from a file of this repository, never logged):
//
//	IDP_BRANT_EER_URL            EER's Runtime API, e.g. http://127.0.0.1:18180
//	IDP_BRANT_EER_TENANT         the EER tenant the credentials map to
//	IDP_BRANT_EER_ACTOR          the EER actor the credentials map to
//	IDP_BRANT_EER_TOKEN          a bearer token, or else:
//	IDP_BRANT_EER_TOKEN_URL      the OAuth 2.0 token endpoint, with
//	IDP_BRANT_EER_CLIENT_ID      the client and
//	IDP_BRANT_EER_CLIENT_SECRET  its secret (client credentials grant), and, for a development
//	IDP_BRANT_EER_USERNAME       identity of an acceptance realm that maps only people to EER,
//	IDP_BRANT_EER_PASSWORD       the password grant
//	IDP_BRANT_EER_CA_FILE        optional PEM CA of an https token endpoint or EER
//	IDP_BRANT_EER_PLAN           the plan the operations run (default address-error-actions),
//	IDP_BRANT_EER_POLICY         the intent policy (default address-exception-policy, version 1),
//	IDP_BRANT_EER_EVENT_TYPE     the originating event type the plan supports (default
//	                             parcel.address.rejected) — the TELOS acceptance configuration has no
//	                             plan of Brant's, so the live runs drive one of its pilot plans with
//	                             synthetic inputs
package eerlive
