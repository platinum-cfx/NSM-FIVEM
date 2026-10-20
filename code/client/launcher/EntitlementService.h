/*
 * This file is part of the Cfx project - https://cfx.re/
 *
 * See LICENSE in the root of the source tree for information
 * regarding licensing.
 */

#pragma once

namespace nsm
{
// NSM's own entitlement service.
//
// The closed-source `legitimacy` component validates the Rockstar entitlement
// against an HTTP endpoint. For Cfx that endpoint is Cfx's own service; an
// NSM account has no record there, so the request is pointed at NSM's local
// service instead (see tools/legitimacy/patch_entitlement_endpoint.py).
//
// The service answers with the very same payload Cfx's service returns for an
// account without an entitlement record. That is the honest answer for NSM
// and it keeps the component on the same code path it used before the
// endpoint was moved.
//
// The Rockstar ownership check is NOT affected by this: it is performed by the
// Rockstar Games Launcher and "does not own a valid license" stays a fatal
// error.
void StartEntitlementService();
}
