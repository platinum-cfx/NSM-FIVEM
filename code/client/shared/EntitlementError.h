#pragma once

#include <string_view>

// NSM: classification of the Rockstar entitlement errors that the
// closed-source "legitimacy" component reports.
//
// The component runs the entitlement validation in the ROS helper process
// (FiveM_ROSLauncher) and in the game process. It ends in a FatalError, which
// terminates the process. When the ROS helper dies, the game process never
// receives its Rockstar session: it blocks in WaitForRlInit() and dies 60
// seconds later, so the "Validating Rockstar Entitlements" window never
// closes and the server browser (which lives in the game process) never
// appears.
//
// Two different things are reported through FatalError by that component:
//
//  1. "the entitlement service could not be consulted" - the transport failed,
//     or the entitlement service has no record for this Rockstar account.
//     NSM accounts never have a Cfx entitlement record, so this is the
//     expected answer for NSM and it must not kill the process. NSM's own
//     local entitlement service (code/client/launcher/EntitlementService.cpp)
//     answers with exactly that "no record" payload.
//
//  2. "this account does not own the game" - the real Rockstar ownership
//     verdict, reported by the Rockstar Games Launcher side of the component
//     ("Could not validate game ownership!", "The Social Club account
//     specified does not own a valid license ..."). That one stays fatal:
//     the ownership check is mandatory.
//
// Only the first class is treated as recoverable. The check itself still runs,
// the error is still reported (log + error pickup file), it is only the
// termination that is suppressed.

inline bool IsEntitlementServiceContactError(std::string_view message)
{
	// "Could not contact entitlement service. Status code: %d, error message: %d/%s, response body: %s"
	if (message.find("Could not contact entitlement service") != std::string_view::npos)
	{
		return true;
	}

	// "Error contacting Rockstar Online Services: %s -> %s."
	// (also seen as part of "Obtained error from CPR: - 403 - Error contacting ...")
	if (message.find("Error contacting Rockstar Online Services") != std::string_view::npos)
	{
		return true;
	}

	return false;
}

// The messages below are the ownership verdict and the Rockstar session
// failures. They must keep terminating the process.
inline bool IsEntitlementOwnershipError(std::string_view message)
{
	static constexpr const char* fatal[] = {
		"Could not validate game ownership",
		"does not own a valid license",
		"Could not sign on to the Social Club",
		"Could not get title access token from the Social Club",
		"Could not get entitlement block from the Social Club",
	};

	for (auto* pattern : fatal)
	{
		if (message.find(pattern) != std::string_view::npos)
		{
			return true;
		}
	}

	return false;
}
