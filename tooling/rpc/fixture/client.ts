import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { EchoService } from "./gen/fixture/v1/echo_pb.js";

const client = createClient(EchoService, createConnectTransport({
  baseUrl: "https://fixture.invalid",
  useBinaryFormat: true,
}));
// Compile method shapes without issuing any requests.
export const echo = () => client.echo({ text: "fixture" });
export const watch = () => client.watch({ text: "fixture" });
