"use strict";

// queueIdOk matches the server's client queue-id rule (validQueueID in
// internal/server/deliver.go): a short token, not message text. A value that
// fails is not written onto the socket.
function queueIdOk(id) {
  return typeof id === "string" && id.length > 0 && id.length <= 64 && /^[A-Za-z0-9._-]+$/.test(id);
}

module.exports = { queueIdOk };
