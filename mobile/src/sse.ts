import { config } from './config';
import { accessToken } from './auth';

export interface StreamEvent {
  type: string;
  consentRequestId: string;
  agentId?: string;
  parentType: string;
  childType: string;
}

// subscribeStream connects to the gateway's /me/stream SSE endpoint and invokes
// onEvent for each consent event. This is the foreground/dev delivery channel
// (NOTIFIER=dev has no external push, so the stream is how prompts arrive); it
// also gives an instantly-updating list when the app is open on any tier.
//
// React Native's fetch does not expose a ReadableStream body, so we read the
// SSE wire incrementally from XMLHttpRequest's progress events. Returns an
// unsubscribe function that aborts the connection.
export function subscribeStream(onEvent: (e: StreamEvent) => void): () => void {
  let xhr: XMLHttpRequest | undefined;
  let closed = false;

  (async () => {
    const token = await accessToken();
    if (!token || closed) return;
    xhr = new XMLHttpRequest();
    xhr.open('GET', `${config.gatewayUrl}/me/stream`);
    xhr.setRequestHeader('Authorization', `Bearer ${token}`);
    xhr.setRequestHeader('Accept', 'text/event-stream');

    let seen = 0;
    xhr.onprogress = () => {
      const text = xhr!.responseText;
      const fresh = text.slice(seen);
      seen = text.length;
      for (const line of fresh.split('\n')) {
        const trimmed = line.trim();
        if (trimmed.startsWith('data: ')) {
          try {
            onEvent(JSON.parse(trimmed.slice('data: '.length)) as StreamEvent);
          } catch {
            // ignore malformed frames / heartbeats
          }
        }
      }
    };
    xhr.send();
  })();

  return () => {
    closed = true;
    xhr?.abort();
  };
}
