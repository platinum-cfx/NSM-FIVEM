/*
 * This file is part of the Cfx project - https://cfx.re/
 *
 * See LICENSE in the root of the source tree for information
 * regarding licensing.
 */

#include "StdInc.h"
#include "EntitlementService.h"

#include <winsock2.h>
#include <ws2tcpip.h>

#include <atomic>
#include <cstdlib>
#include <cstring>
#include <string>
#include <thread>

namespace
{
constexpr const char* kBindAddress = "127.0.0.1";

// This has to match the endpoint the entitlement check is patched to point
// at - see tools/legitimacy/patch_entitlement_endpoint.py.
constexpr unsigned short kBindPort = 18080;

// The payload Cfx's own entitlement service returns for an account that has
// no entitlement record. NSM accounts are not Cfx accounts, so this is the
// honest answer, and it is byte for byte the answer the launcher used to get
// from Cfx before the endpoint was moved to NSM.
constexpr const char* kNoEntitlementRecord =
	"<?xml version=\"1.0\" encoding=\"utf-8\"?><Response><Status>0</Status><Error Code=\"DoesNotExist\" /></Response>";

constexpr const char* kNotFound =
	"<?xml version=\"1.0\" encoding=\"utf-8\"?><Response><Status>0</Status><Error Code=\"NotFound\" /></Response>";

constexpr const char* kContentType = "text/xml; charset=utf-8";

std::atomic<bool> g_running{ true };

bool SendAll(SOCKET socket, const char* data, size_t length)
{
	size_t sent = 0;

	while (sent < length)
	{
		auto result = send(socket, data + sent, (int)(length - sent), 0);

		if (result <= 0)
		{
			return false;
		}

		sent += (size_t)result;
	}

	return true;
}

void Respond(SOCKET client, int status, const char* reason, const char* body)
{
	std::string response;

	response += "HTTP/1.1 ";
	response += std::to_string(status);
	response += " ";
	response += reason;
	response += "\r\n";
	response += "Content-Type: ";
	response += kContentType;
	response += "\r\n";
	response += "Content-Length: ";
	response += std::to_string(strlen(body));
	response += "\r\n";
	response += "Cache-Control: no-store\r\n";
	response += "Connection: close\r\n";
	response += "\r\n";
	response += body;

	SendAll(client, response.c_str(), response.size());
}

// Reads the request (headers and body) and answers it.
void HandleConnection(SOCKET client)
{
	std::string request;
	char buffer[4096];

	// read until the end of the headers
	size_t headerEnd = std::string::npos;

	while (headerEnd == std::string::npos)
	{
		auto result = recv(client, buffer, sizeof(buffer), 0);

		if (result <= 0)
		{
			return;
		}

		request.append(buffer, (size_t)result);

		headerEnd = request.find("\r\n\r\n");

		if (request.size() > 64 * 1024)
		{
			return;
		}
	}

	// read the body, if the request announced one
	size_t contentLength = 0;
	auto contentLengthPos = request.find("Content-Length:");

	if (contentLengthPos != std::string::npos)
	{
		contentLength = (size_t)strtoul(request.c_str() + contentLengthPos + 15, nullptr, 10);
	}

	while (request.size() < headerEnd + 4 + contentLength)
	{
		auto result = recv(client, buffer, sizeof(buffer), 0);

		if (result <= 0)
		{
			break;
		}

		request.append(buffer, (size_t)result);
	}

	// the request line looks like "GET /l/api/validate/entitlement HTTP/1.1"
	std::string path;

	if (request.size() > 4)
	{
		auto firstSpace = request.find(' ');

		if (firstSpace != std::string::npos)
		{
			auto secondSpace = request.find(' ', firstSpace + 1);

			path = request.substr(firstSpace + 1,
				(secondSpace == std::string::npos) ? std::string::npos : secondSpace - firstSpace - 1);
		}
	}

	// strip the query string
	auto queryPos = path.find('?');

	if (queryPos != std::string::npos)
	{
		path = path.substr(0, queryPos);
	}

	// every entitlement endpoint of the component ends like this
	const std::string prefix = "/l/api/validate/entitlement";

	if (path.compare(0, prefix.size(), prefix) == 0)
	{
		trace("NSM entitlement service: %s -> no entitlement record\n", path.c_str());
		Respond(client, 200, "OK", kNoEntitlementRecord);
		return;
	}

	trace("NSM entitlement service: unknown path %s\n", path.c_str());
	Respond(client, 404, "Not Found", kNotFound);
}

void ServiceLoop(SOCKET listener)
{
	while (g_running)
	{
		SOCKET client = accept(listener, nullptr, nullptr);

		if (client == INVALID_SOCKET)
		{
			if (!g_running)
			{
				break;
			}

			continue;
		}

		HandleConnection(client);

		shutdown(client, SD_BOTH);
		closesocket(client);
	}
}
}

namespace nsm
{
void StartEntitlementService()
{
	WSADATA wsaData;

	if (WSAStartup(MAKEWORD(2, 2), &wsaData) != 0)
	{
		trace("NSM entitlement service: WSAStartup failed, entitlement check will fail\n");
		return;
	}

	SOCKET listener = socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);

	if (listener == INVALID_SOCKET)
	{
		trace("NSM entitlement service: socket failed (%d)\n", WSAGetLastError());
		return;
	}

	int reuse = 1;
	setsockopt(listener, SOL_SOCKET, SO_REUSEADDR, (const char*)&reuse, sizeof(reuse));

	sockaddr_in address = { 0 };
	address.sin_family = AF_INET;
	address.sin_port = htons(kBindPort);
	address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);

	if (bind(listener, (sockaddr*)&address, sizeof(address)) == SOCKET_ERROR)
	{
		trace("NSM entitlement service: could not bind %s:%d (%d), entitlement check will fail\n", kBindAddress, (int)kBindPort, WSAGetLastError());
		closesocket(listener);
		return;
	}

	if (listen(listener, 16) == SOCKET_ERROR)
	{
		trace("NSM entitlement service: listen failed (%d)\n", WSAGetLastError());
		closesocket(listener);
		return;
	}

	trace("NSM entitlement service: listening on %s:%d\n", kBindAddress, (int)kBindPort);

	std::thread([] (SOCKET listener)
	{
		ServiceLoop(listener);
		closesocket(listener);
	}, listener).detach();
}
}
