const relays = ["wss://relay.example"];
const request = ["REQ", "timeline", { kinds: [1] }];
function onRelayMessage(message: unknown[]) {
  if (message[0] === "EOSE") closeSubscription();
}
