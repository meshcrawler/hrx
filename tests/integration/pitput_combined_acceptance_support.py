"""Narrow real-provider observations for frozen combined acceptance (#30)."""

import copy
import hashlib
import importlib.util
import json
import os
import re
import threading
import time
from pathlib import Path

from pitput_combined_launch import Provider
from pitput_exchange import private


def provider_reply_facts(reply, operation):
    """Only fixed classifications and hashes; no tool arguments or model text."""
    result = {'kind': 'completion' if isinstance(reply, str) else 'other',
              'operation': operation if operation in ('read', 'reply', 'ack') else 'other',
              'http_write_returned': False}
    if isinstance(reply, dict) and reply.get('type') == 'function_call':
        result['kind'] = 'tool'
        result['tool'] = reply.get('name') if reply.get('name') in (
            'shell', 'shell_command', 'exec_command') else 'other'
        for source, target in (('call_id', 'call_sha256'), ('arguments', 'arguments_sha256')):
            value = reply.get(source)
            if isinstance(value, str):
                result[target] = hashlib.sha256(value.encode()).hexdigest()
    return result


def safe_transport_observation(value):
    result = {key: value.get(key) if type(value.get(key)) is bool else None for key in (
        'transport_healthy', 'controller_healthy', 'native_idle', 'observation_deferred')}
    pending = value.get('pending')
    result['pending'] = None
    if pending is not None:
        item = pending if isinstance(pending, dict) else {}
        result['pending'] = {
            'delivery': item.get('delivery') if item.get('delivery') in (
                'received', 'enqueueing', 'notified', 'delivery_uncertain', 'delivery_failed') else 'other',
            'ack_requested': item.get('ack_requested') if type(item.get('ack_requested')) is bool else None,
            'attempts': item.get('attempts') if type(item.get('attempts')) is int
                and 0 <= item['attempts'] <= 2**63 - 1 else None,
        }
    return result


def policy_transition_selection(current, *, owner, mode=None):
    """Explicit fixture-only legacy mention -> mandatory addressed-floor migration.

    Plain preserve/withdraw retain their exact selection. Mode transitions require
    the known equivalent owner-mention/self-exclusion filter; other legacy
    selectors cannot be silently discarded. No ordinary floor inclusion is added.
    """
    selection = copy.deepcopy(current)
    if mode is None:
        return selection
    if mode not in ('enabled', 'muted', 'disabled'):
        raise ValueError('invalid_acceptance_policy_mode')
    spec = json.loads(selection['board_filter_json'])
    if spec.get('path') != selection.get('board_path'):
        raise ValueError('acceptance_policy_board_changed')
    raw_policy = selection.get('attention_policy_json')
    if raw_policy is None:
        if (spec.get('mentions') != [owner.casefold()]
                or spec.get('exclude_authors') != [owner]
                or any(spec.get(key) for key in ('threads', 'followed_threads', 'regex'))
                or spec.get('dm', 'none') != 'none' or spec.get('dm_session') is not None):
            raise ValueError('acceptance_floor_migration_requires_exact_owner_target')
        # board_context uses the same exact owner mention matcher as MonitorSpec.
        # With no ordinary rules, the mandatory floor keeps owner mentions and
        # excludes self-authored board items; unaddressed controls do not select.
        spec['mentions'], spec['exclude_authors'] = [], []
        selection['board_filter_json'] = json.dumps(spec, sort_keys=True)
        policy = {'rules': []}
    else:
        policy = json.loads(raw_policy)
        if (policy.get('rules', [])
                or any(spec.get(key) for key in
                       ('threads', 'followed_threads', 'mentions', 'regex', 'exclude_authors'))):
            raise ValueError('acceptance_floor_policy_changed')
    policy['mode'] = mode
    selection['attention_policy_json'] = json.dumps(policy, sort_keys=True)
    return selection


def policy_transition_payload(current, *, owner, mode=None, action='preserve'):
    if action not in ('preserve', 'withdraw'):
        raise ValueError('invalid_acceptance_pending_action')
    return {
        'request_id': 'acceptance-policy-' + str(current['version']),
        'expected_version': current['version'],
        'reason': 'Frozen acceptance explicit policy change',
        'selection': policy_transition_selection(current['selection'], owner=owner, mode=mode),
        'pending_action': action,
    }


def publish_control(path, text):
    """Publish a complete private control record to the concurrent reader."""
    temporary = path.with_name(path.name + '.tmp')
    private(temporary, text)
    temporary.replace(path)


def source_module(name):
    path = Path(os.environ['HRX_PITPUT_OVERRIDE_WORKDIR']) / 'source/scripts' / (name + '.py')
    spec = importlib.util.spec_from_file_location('acceptance_' + name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def json_digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def persisted_payload_matches(fact, event):
    """Audit metadata carries exact payload hash/bytes; escrowed text is separate."""
    from ysg_mail.presentation_retention import NotificationReference

    reference = NotificationReference.model_validate_json(json.dumps({
        key: value for key, value in fact.items()
        if key not in ('payload_sha256', 'payload_bytes')}))
    return (reference.disposition == 'notification_accepted'
            and fact['payload_sha256'] == event['payload_sha256']
            and type(fact['payload_bytes']) is int
            and fact['payload_bytes'] == event['notification_utf8_bytes'])


def bound_admission_attempt(prepared, receipt, text):
    """Read attempt from the exact durable owned-admission record, not Metadata.

    Publication metadata intentionally has no attempt. dispatch_owned durably
    saves this enqueueing Admission before invoking the sole native owner.
    The caller parses both values with the canonical Pitput models first.
    """
    if (prepared.receipt != receipt or receipt.presentation.text != text
            or prepared.receipt.delivery_id != receipt.delivery_id
            or type(prepared.attempt) is not int or prepared.attempt < 1
            or prepared.phase not in ('enqueueing', 'uncertain', 'admitted')):
        raise ValueError('notification_admission_attempt_binding')
    return prepared.attempt


def lost_ack_evidence(record, *, phase, boundary, tombstones, acknowledgments, owner):
    """Positive crash-only handling proof, never a synthetic native/tool receipt.

    The owning CLI writes its exact event tombstone only after explicit ACK intent
    and a validated service ACK (codex_bridge._acknowledge_pending). Source absence
    and admission uncertainty provide no evidence here.
    """
    if phase not in ('after-admission', 'queue_response') or record['source'] != 'dm':
        raise ValueError('receipt_loss_boundary_refused')
    admitted = boundary.get('owned_outcome', {}) if phase == 'after-admission' else boundary
    if (admitted.get('delivery_id') != record['bridge_delivery_id']
            or admitted.get('payload_sha256') != record['payload_sha256']
            or admitted.get('attempt') != record['attempt']
            or type(admitted.get('response_ordinal')) is not int
            or admitted['response_ordinal'] <= 0
            or (phase == 'after-admission' and admitted.get('status') != 'admitted')
            or boundary.get('phase') != phase):
        raise ValueError('receipt_loss_admission_binding')
    issued = record.get('ack_command_issued', {})
    call_key = f"{record['bridge_delivery_id']}:ack:{record.get('read_body_utf8_bytes', 0)}"
    expected_call = 'call_' + hashlib.sha256(call_key.encode()).hexdigest()[:24]
    if (record['completed'] or record.get('helper_ack') is not None
            or issued.get('call_id') != expected_call or not issued.get('arguments_sha256')
            or record.get('read_sha256') != record['expected_read_sha256']
            or not record.get('read_body_utf8_bytes')
            or not record.get('read_json_utf8_bytes')):
        raise ValueError('receipt_loss_explicit_read_ack_required')
    matched = [event for event in tombstones if event.get('event_key') == record['delivery_id']]
    if len(matched) != 1 or json_digest(matched[0]) != record['bridge_event_sha256']:
        raise ValueError('receipt_loss_exact_tombstone_required')
    if len(acknowledgments) != 1:
        raise ValueError('receipt_loss_unique_service_ack_required')
    acknowledgment = acknowledgments[0]
    expected = {'protocol': 'combined-attention.v1', 'delivery_id': record['delivery_id'],
                'request_id': 'handle-' + record['delivery_id'],
                'reason': 'Explicit owning-bridge handling acknowledgment', 'status': 'acknowledged'}
    if (acknowledgment['record'] != expected
            or json_digest(acknowledgment['delivery']) != record['selected_envelope_sha256']
            or matched[0]['combined'] != acknowledgment['delivery']
            or issued.get('owner') != owner or issued.get('delivery_id') != record['delivery_id']):
        raise ValueError('receipt_loss_exact_service_ack_required')
    return {'status': 'receipt_lost_reconciled', 'phase': phase,
            'service_ack_request_id': expected['request_id'],
            'native_ack_call_id': issued['call_id'],
            'native_ack_arguments_sha256': issued['arguments_sha256'],
            'bridge_event_sha256': record['bridge_event_sha256'],
            'service_ack_observed': True, 'provider_receipt_observed': False,
            'handling_effects': 1}


class ExpectedOwnedCancellation(RuntimeError):
    pass


class OwnedReadResumeWitness:
    """One physical READ recovery, authorized by an actual stopped-owner boundary."""

    def __init__(self, *, phase, boundary, stopped, record, receipt, pending, offset,
                 validated_read_bytes, operation):
        outcome = boundary.get('owned_outcome', {}) if phase == 'after-admission' else boundary
        positive = (boundary.get('helper_invoked') is True and boundary.get('helper_exit') == 0
                    and outcome.get('status') == 'admitted') if phase == 'after-admission' else (
                        phase == 'queue_response' and boundary.get('thread_id') == record['session'])
        digest = hashlib.sha256(receipt.presentation.text.encode()).hexdigest()
        if (phase not in ('after-admission', 'queue_response') or boundary.get('phase') != phase
                or not positive or stopped.get('owner_stopped') is not True
                or stopped.get('thread') != record['session']
                or outcome.get('delivery_id') != record['delivery_id']
                or record['bridge_delivery_id'] != receipt.delivery_id
                or outcome.get('payload_sha256') != digest or record['payload_sha256'] != digest
                or type(outcome.get('attempt')) is not int or outcome['attempt'] != record['attempt']
                or outcome['attempt'] < 1 or type(outcome.get('response_ordinal')) is not int
                or outcome['response_ordinal'] <= 0 or operation != 'read'
                or not isinstance(pending, str) or not pending
                or type(offset) is not int or offset < 0
                or type(validated_read_bytes) is not int or offset != validated_read_bytes):
            raise ValueError('owned_read_resume_witness_invalid')
        self.receipt, self.pending, self.offset, self.digest = receipt, pending, offset, digest
        self.used = False

    def __call__(self, receipt, pending, offset, text):
        previous = self.used
        self.used = True
        if (previous or receipt != self.receipt or pending != self.pending
                or type(offset) is not int or offset != self.offset
                or text != self.receipt.presentation.text
                or hashlib.sha256(text.encode()).hexdigest() != self.digest):
            raise ValueError('owned_read_resume_witness_mismatch')
        return 'owned_stopped_user_replayed_pending_read'


class MatrixProvider(Provider):
    """Same scripted adapter/tools; independent labeled inputs and actual read bytes."""

    def __init__(self, cwd):
        super().__init__(cwd)
        self.counter = source_module('native_quiescence').ProviderCounter()
        self.expected = {}
        self.records = []
        self.expected_busy_cancellations = 0
        self.expected_attention_cancellations = 0
        self.attention_release = threading.Event()
        self.attention_seen = threading.Event()
        self.cancel_paused_attention = False
        self.attention_release.set()
        self.whole_source = []
        self.busy = threading.Event()
        self.busy_seen = threading.Event()
        self.busy.set()
        self._read_parts = []
        self._active_record = None
        self.tool_diagnostics = []
        self.ack_retry_authorizer = None
        self.read_resume_authorizer = None
        original = self.engine
        original.inner.responder.body_byte_limit = 256
        original.inner.max_requests = 256
        original.respond = self._observe_response(original.respond)
        handler = self.server.RequestHandlerClass
        counter = self.counter

        class CountedHandler(handler):
            def do_POST(self):
                busy_request = False
                try:
                    size = int(self.headers.get('Content-Length', '0'))
                    if not 0 < size <= 1000000:
                        raise ValueError('bounded_request_required')
                    request = json.loads(self.rfile.read(size))
                    normalized = self_provider.module.normalize(request)
                    busy_request = normalized['messages'][-1].get('content') == 'fixture-authorized-busy-turn'
                    reply = self_provider.engine.respond(normalized)
                    record = self_provider._active_record
                    facts = provider_reply_facts(reply, self_provider.engine.inner.responder.operation)
                    if record is not None and not busy_request:
                        record['last_provider_reply'] = facts
                    if (isinstance(reply, str) and self_provider.engine.facts()['completed']
                            and reply != 'Authorized foreground fixture completed.'):
                        kind = self_provider.seen[-1].header.source
                        self_provider.completed.append(kind)
                        reply += '\nfixture-combined-complete:' + kind
                    events = self_provider.module.response_events(reply, 'combined-acceptance')
                    body = ''.join('data: ' + json.dumps(event) + '\n\n' for event in events).encode()
                    self.send_response(200)
                    self.send_header('Content-Type', 'text/event-stream')
                    self.send_header('Content-Length', str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                    facts['http_write_returned'] = True
                except Exception as error:
                    if isinstance(error, ExpectedOwnedCancellation):
                        self_provider.expected_attention_cancellations += 1
                        self.close_connection = True
                    elif busy_request and isinstance(error, (BrokenPipeError, ConnectionResetError)):
                        self_provider.expected_busy_cancellations += 1
                    else:
                        code = str(error)
                        detail = ':' + code if re.fullmatch('[a-z0-9_]{1,80}', code) else ''
                        self_provider.errors.append(type(error).__name__ + detail)
                        self.send_response(500)
                        self.end_headers()

            def parse_request(self):
                self.counting = False
                result = super().parse_request()
                if result:
                    counter.begin()
                    self.counting = True
                return result

            def handle_one_request(self):
                try:
                    super().handle_one_request()
                finally:
                    if getattr(self, 'counting', False):
                        self.counting = False
                        counter.end()

        self_provider = self
        self.server.RequestHandlerClass = CountedHandler

    def close(self):
        self.cancel_paused_attention = True
        self.attention_release.set()
        self.busy.set()
        super().close()

    def _observe_response(self, respond):
        def observed(request):
            last = request['messages'][-1]
            if last.get('role') == 'user' and last.get('content') == 'fixture-authorized-busy-turn':
                self.busy_seen.set()
                if not self.busy.wait(60):
                    raise ValueError('busy_fixture_release_deadline')
                return 'Authorized foreground fixture completed.'
            if last.get('role') == 'user' and not self.attention_release.is_set():
                self.attention_seen.set()
                if not self.attention_release.wait(60):
                    raise ValueError('owned_attention_pause_deadline')
                if self.cancel_paused_attention:
                    raise ExpectedOwnedCancellation()
            responder = self.engine.inner.responder
            framed = result = operation = None
            if last.get('role') == 'user' and self.read_resume_authorizer is not None:
                retry = self.engine.retry_interrupted_read(
                    request, authorize_user_replay=self.read_resume_authorizer)
                if retry is not None:
                    self._active_record['interrupted_read_retries'] = (
                        self._active_record.get('interrupted_read_retries', 0) + 1)
                    self._active_record.setdefault('read_recovery_events', []).append(
                        self.engine.read_recovery_events[-1])
                    return retry
            if last.get('role') == 'tool':
                self.tool_diagnostics.append(self.engine.tool_diagnostic(request))
                del self.tool_diagnostics[:-64]  # Retain the current failure after long fairness runs.
                retry = self.engine.retry_interrupted_read(request)
                if retry is not None:
                    self._active_record['interrupted_read_retries'] = (
                        self._active_record.get('interrupted_read_retries', 0) + 1)
                    self._active_record.setdefault('read_recovery_events', []).append(
                        self.engine.read_recovery_events[-1])
                    return retry
                retry_ack = self.engine.retry_faulted_ack(
                    request, authorize=self.ack_retry_authorizer)
                if retry_ack is not None:
                    self._active_record.setdefault('ack_recovery_events', []).append(
                        self.engine.ack_recovery_events[-1])
                    return retry_ack
                # Reject wrong actual tool IDs before decoding their stdout.
                if last.get('tool_call_id') != responder.call_id:
                    raise ValueError('uncorrelated_fixture_tool_result')
                framed = self.module.opencode.envelope.decode(
                    responder._text(last['content']), responder.call_id)
                result = json.loads(framed['stdout'])
                operation = responder.operation
            reply = respond(request)
            # Independent byte/effect observations follow successful exact-fragment
            # validation. Failed/old native results never count as another read.
            if framed is not None:
                if operation == 'read':
                    self._read_parts.append(result['body_utf8'].encode())
                    self._active_record['read_json_utf8_bytes'] += len(framed['stdout'].encode())
                elif operation == 'reply':
                    self._active_record['recipient_reply_version'] = result['version']
                elif operation == 'ack':
                    self._active_record['helper_ack'] = result
            if (isinstance(reply, dict) and reply.get('type') == 'function_call'
                    and responder.operation == 'ack'):
                actual = b''.join(self._read_parts)
                expected = self.expected[self._active_record['source_key']]['snapshot'].body.encode()
                if actual != expected or reply.get('call_id') != responder.call_id:
                    raise ValueError('explicit_ack_requires_exact_validated_reads')
                self._active_record.update(
                    read_sha256=hashlib.sha256(actual).hexdigest(),
                    read_body_utf8_bytes=len(actual),
                    ack_command_issued={'call_id': reply['call_id'],
                        'arguments_sha256': hashlib.sha256(reply['arguments'].encode()).hexdigest(),
                        'owner': responder.expected.owner,
                        'delivery_id': self._active_record['delivery_id']})
            if isinstance(reply, str) and self.engine.facts()['completed']:
                record = self._active_record
                actual = b''.join(self._read_parts)
                expected = self.expected[record['source_key']]['snapshot'].body.encode()
                if actual != expected:
                    raise ValueError('independent_exact_read_bytes_mismatch')
                record.update(read_sha256=hashlib.sha256(actual).hexdigest(),
                              read_body_utf8_bytes=len(actual), completed=True,
                              post_ack_continuation=True)
                self._read_parts = []
            return reply
        return observed

    def arm_read_resume(self, phase, boundary, stopped):
        self.read_resume_authorizer = None
        with self.engine.inner._lock:
            responder = self.engine.inner.responder
            # No pending model READ means the ordinary first delivery path must
            # run. Do not invent an expectation or wait for a helper before stop.
            if (self._active_record is None or responder.call_id is None
                    or responder.operation != 'read' or responder.completed):
                return
            self.read_resume_authorizer = OwnedReadResumeWitness(
                phase=phase, boundary=boundary, stopped=stopped, record=self._active_record,
                receipt=responder.expected, pending=responder.call_id, offset=responder.offset,
                validated_read_bytes=sum(map(len, self._read_parts)), operation=responder.operation)

    def retire_reconciled_ack(self, record, evidence):
        """Retire an exact completed logical delivery without inventing model success."""
        responder = self.engine.inner.responder
        if (self._active_record is not record or self.counter.snapshot()[1] != 0
                or responder.completed or responder.operation != 'ack'
                or responder.call_id != record['ack_command_issued']['call_id']
                or evidence.get('status') != 'receipt_lost_reconciled'):
            raise ValueError('receipt_loss_retirement_refused')
        record['receipt_lost_reconciled'] = evidence
        # No completed flag, helper result or post-ACK inference is manufactured.
        # expect() may now accept the next genuinely new notification.
        responder.expected = responder.snapshot = None
        responder.call_id = responder.operation = None
        self._read_parts = []

    def arm(self, key, label, snapshot):
        if key in self.expected:
            raise ValueError('fixture_duplicate_source_key')
        self.expected[key] = {'label': label, 'snapshot': snapshot}
        self.whole_source.append(snapshot.body.encode())

    def resolve(self, text):
        from ysg_mail import codex_bridge as bridge
        from ysg_mail.attention_protocol import Admission, Metadata, Receipt
        config = bridge._load_config(self.config)
        store = bridge.Store(config, config.agents[0])
        metadata = Metadata.model_validate_json((store.directory / 'attention-metadata.json').read_bytes())
        receipt = Receipt.model_validate_json((store.directory / 'attention-presentation.json').read_bytes())
        event = bridge._event(metadata.event, store.agent)
        if text != receipt.presentation.text or receipt.header.combined_delivery_id != event['event_key']:
            raise ValueError('exact_combined_notice_required')
        bridge._validate_binding(store, event, metadata.origin)
        prepared = Admission.model_validate_json(
            (store.directory / 'attention-admission.json').read_bytes())
        attempt = bound_admission_attempt(prepared, receipt, text)
        delivery = event['combined']
        if delivery['source'] == 'ask':
            notice = delivery['notice']['selection']['notice']
            key = (notice['ask_id'], notice['ask_version'])
        else:
            key = delivery['attention']['event_key']
        deadline = time.monotonic() + 10
        while key not in self.expected and time.monotonic() < deadline:
            time.sleep(0.01)
        if key not in self.expected:
            raise ValueError('unexpected_or_stale_source_delivery')
        expected = self.expected[key]
        snapshot = expected['snapshot']
        if delivery['source'] == 'ask':
            if expected['typed'].snapshot_version != notice['snapshot_version']:
                raise ValueError('typed_snapshot_changed')
            snapshot = snapshot.model_copy(update={'event_key': notice['notice_id'],
                                                   'sender': notice['actor']})
            expected['snapshot'] = snapshot
        else:
            attention = delivery['attention']
            sequence = attention['inbox_seq' if delivery['source'] == 'dm' else 'seq']
            if snapshot.sequence != sequence or snapshot.event_key != attention['event_key']:
                raise ValueError('independent_source_binding_changed')
        if any(record['source_key'] == key for record in self.records):
            raise ValueError('duplicate_native_handling_effect')
        self.engine.inner.responder.reply_to_asks = (
            snapshot.source == 'ask' and json.loads(snapshot.body)['state']['status'] == 'open')
        self._active_record = {
            'label': expected['label'], 'source_key': key, 'source': snapshot.source,
            'source_event_key': snapshot.event_key, 'session': store.agent.thread_id,
            'delivery_id': delivery['delivery_id'], 'source_monitor': delivery['source_monitor'],
            'selection': delivery.get('notice') or delivery['attention'],
            'bridge_delivery_id': receipt.delivery_id,
            'bridge_event_sha256': json_digest(event),
            'selected_envelope_sha256': json_digest(delivery),
            'attempt': attempt,
            'notification_utf8_bytes': len(text.encode()),
            'payload_sha256': hashlib.sha256(text.encode()).hexdigest(),
            'expected_read_sha256': hashlib.sha256(snapshot.body.encode()).hexdigest(),
            'read_json_utf8_bytes': 0, 'completed': False,
        }
        self.records.append(self._active_record)
        self.seen.append(receipt)
        return receipt, snapshot


ADMISSION_FAULT_WRAPPER = '''#!/usr/bin/env python3
# Disposable fixture wrapper: the real installed production admission helper runs.
import json,os,subprocess,sys
from pathlib import Path
root=Path(os.environ['HRX_PITPUT_COMBINED_CONTROL'])
def publish_record(record):
 temporary=root/'fault-entered.json.tmp'
 temporary.write_text(json.dumps(record))
 temporary.chmod(0o600)
 temporary.replace(root/'fault-entered.json')
mode_file=root/'admission-fault.json'
mode=json.loads(mode_file.read_text()) if mode_file.exists() else {}
phase=mode.get('phase')
if phase in {'before-admission','after-admission'}:
 mode_file.unlink()
 record={'phase':phase,'helper_invoked':False}
 if phase=='before-admission':
  publish_record(record)
  os._exit(73)
 result=subprocess.run([os.environ['HRX_PITPUT_REAL_ADMISSION_BINARY'],*sys.argv[1:]],capture_output=True)
 record.update(helper_invoked=True,helper_exit=result.returncode)
 if result.returncode==0:
  outcome=json.loads(result.stdout)
  record['owned_outcome']={key:outcome.get(key) for key in ('status','attempt','delivery_id','response_ordinal','payload_sha256')}
 publish_record(record)
 os._exit(74)
os.execv(os.environ['HRX_PITPUT_REAL_ADMISSION_BINARY'],[os.environ['HRX_PITPUT_REAL_ADMISSION_BINARY'],*sys.argv[1:]])
'''


class AckFaultProxy:
    """Real loopback HTTP transport loss immediately before/after actual ACK commit."""

    def __init__(self, target):
        import http.client
        import http.server
        from urllib.parse import urlsplit
        self.mode = None
        self.observation = None
        self.restored = threading.Event()
        self.recovery_consumed = False
        self.lock = threading.Lock()
        self.hit = threading.Event()
        self.release = threading.Event()
        self.release.set()
        parsed = urlsplit(target)
        proxy = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                self.forward()

            do_POST = do_GET
            do_PUT = do_GET
            do_DELETE = do_GET

            def forward(self):
                body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
                mode = None
                if self.path.endswith('/combined/ack'):
                    with proxy.lock:
                        mode, proxy.mode = proxy.mode, None
                    if mode is not None:
                        intent = json.loads(body)
                        target = {'delivery_id': intent.get('delivery_id'),
                                  'request_id': intent.get('request_id')}
                    if mode == 'before-ack-commit':
                        proxy.observation = {'phase': mode, 'forwarded': False, **target}
                        proxy.hit.set()
                        self.close_connection = True
                        return
                conn = http.client.HTTPConnection(parsed.hostname, parsed.port, timeout=30)
                try:
                    headers = {key: value for key, value in self.headers.items()
                               if key.lower() not in {'host', 'connection'}}
                    conn.request(self.command, self.path, body=body, headers=headers)
                    response = conn.getresponse()
                    content = response.read()
                    if mode in {'after-ack-commit', 'lost-ack-response'}:
                        outcome = json.loads(content)
                        proxy.observation = {'phase': mode, 'forwarded': True, **target,
                                             'http_status': response.status,
                                             'acknowledgment': outcome}
                        proxy.hit.set()
                        self.close_connection = True
                        return
                    self.send_response(response.status)
                    self.send_header('Content-Type', 'application/json')
                    self.send_header('Content-Length', str(len(content)))
                    self.end_headers()
                    self.wfile.write(content)
                finally:
                    conn.close()

        self.server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = f'http://127.0.0.1:{self.server.server_port}'

    def arm(self, phase):
        if phase not in {'before-ack-commit', 'after-ack-commit', 'lost-ack-response'}:
            raise ValueError('unknown_ack_fault')
        with self.lock:
            self.observation = None
            self.recovery_consumed = False
            self.restored.clear()
            self.hit.clear()
            self.mode = phase

    def mark_restored(self, phase):
        """Trusted fixture calls only after restarted service health succeeds."""
        with self.lock:
            if (phase not in {'before-ack-commit', 'after-ack-commit', 'lost-ack-response'}
                    or self.observation is None or self.observation['phase'] != phase
                    or not self.hit.is_set()):
                raise ValueError('fixture_ack_restore_boundary_missing')
            self.restored.set()

    @staticmethod
    def _exact_fault(observed, delivery_id):
        if (observed is None or observed.get('delivery_id') != delivery_id
                or observed.get('request_id') != 'handle-' + delivery_id):
            return False
        phase = observed.get('phase')
        if phase == 'before-ack-commit':
            return observed.get('forwarded') is False
        acknowledgment = observed.get('acknowledgment', {})
        if not isinstance(acknowledgment, dict):
            return False
        return (phase in {'after-ack-commit', 'lost-ack-response'}
                and observed.get('forwarded') is True and observed.get('http_status') == 200
                and acknowledgment.get('protocol') == 'combined-attention.v1'
                and acknowledgment.get('status') == 'acknowledged'
                and acknowledgment.get('delivery_id') == delivery_id
                and acknowledgment.get('request_id') == observed['request_id'])

    def authorize_failed_ack(self, delivery_id):
        """One exact captured ACK intent, after real restoration; never success."""
        with self.lock:
            observed = self.observation
            if not self._exact_fault(observed, delivery_id):
                raise ValueError('fixture_ack_fault_target_mismatch')
        if not self.restored.wait(45):
            raise ValueError('fixture_ack_restore_deadline')
        with self.lock:
            if self.observation is not observed or self.recovery_consumed:
                raise ValueError('fixture_ack_fault_authorization_consumed')
            self.recovery_consumed = True
            return observed['phase']

    def close(self):
        self.release.set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(5)
