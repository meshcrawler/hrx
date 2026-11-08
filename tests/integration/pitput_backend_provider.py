"""Scripted backend tools for the shared combined acceptance transcript.

No runtime is started here. OpenCode keeps Chat Completions shell calls; Claude
keeps Messages Bash/background-task notifications. Neither uses Codex ingress.
"""

import hashlib
import http.server
import json
from pathlib import PurePosixPath
import re
import shlex
import threading
import time

from pitput_combined_acceptance_support import json_digest, source_module


class OwnedGateCancellation(Exception):
    pass


def native_reply_delivery(reply, operation):
    facts = {'kind': 'other', 'operation': operation if operation in ('read', 'reply', 'ack') else 'other',
             'http_write_returned': False}
    calls = reply.get('tool_calls') if isinstance(reply, dict) else None
    if isinstance(calls, list) and len(calls) == 1:
        call = calls[0]
        function = call.get('function', {})
        if isinstance(call.get('id'), str) and isinstance(function.get('arguments'), str):
            facts.update(kind='tool', call_sha256=hashlib.sha256(call['id'].encode()).hexdigest(),
                         arguments_sha256=hashlib.sha256(function['arguments'].encode()).hexdigest(),
                         tool=function.get('name') if function.get('name') in ('shell', 'bash') else 'other')
    return facts


def native_request_facts(request, emitted_call_hash=None):
    roles = {name: 0 for name in ('system', 'user', 'assistant', 'tool', 'other')}
    if not isinstance(request, dict):
        return {'routing_class': 'rejected_or_unparsed', 'tools_present': False,
                'streaming': False,
                'message_roles': roles, 'last_role': 'other', 'tool_result_count': 0,
                'last_tool_result_count': 0, 'last_call_sha256': None, 'matches_emitted_call': False}
    messages = request.get('messages')
    messages = messages if isinstance(messages, list) else []

    def result_ids(message):
        result = [message.get('tool_call_id')] if message.get('role') == 'tool' else []
        content = message.get('content')
        if isinstance(content, list):
            result += [block.get('tool_use_id') for block in content if isinstance(block, dict)
                       and block.get('type') == 'tool_result']
        return result

    count = 0
    for message in messages:
        if not isinstance(message, dict):
            roles['other'] += 1
            continue
        role = message.get('role')
        roles[role if isinstance(role, str) and role in roles else 'other'] += 1
        count += len(result_ids(message))
    last = messages[-1] if messages and isinstance(messages[-1], dict) else {}
    identifiers = result_ids(last)
    digest = (hashlib.sha256(identifiers[0].encode()).hexdigest()
              if len(identifiers) == 1 and isinstance(identifiers[0], str) else None)
    return {'routing_class': 'responder' if request.get('tools') else 'metadata',
            'streaming': request.get('stream') is True,
            'tools_present': bool(request.get('tools')), 'message_roles': roles,
            'last_role': last.get('role') if isinstance(last.get('role'), str) and last.get('role') in roles else 'other',
            'tool_result_count': count, 'last_tool_result_count': len(identifiers),
            'last_call_sha256': digest,
            'matches_emitted_call': digest is not None and digest == emitted_call_hash}


def owned_initialization_tool_suffix(request, pending_call):
    """Remove only the verified final init user; never search older history."""
    messages = request.get('messages')
    if not isinstance(messages, list) or len(messages) < 3:
        raise ValueError('owned_initialization_pending_tool_suffix_missing')
    assistant, result, initialization = messages[-3:]
    if (not all(isinstance(item, dict) for item in (assistant, result, initialization))
            or assistant.get('role') != 'assistant' or result.get('role') != 'tool'
            or initialization.get('role') != 'user'):
        raise ValueError('owned_initialization_pending_tool_suffix_invalid')
    calls = assistant.get('tool_calls')
    if (not isinstance(calls, list) or len(calls) != 1 or not isinstance(calls[0], dict)
            or calls[0].get('id') != pending_call or result.get('tool_call_id') != pending_call):
        raise ValueError('owned_initialization_pending_tool_call_mismatch')
    return {**request, 'messages': messages[:-1]}


def verify_crash_native_proof(proof, binding):
    if not isinstance(proof, dict):
        raise ValueError('crash_witness_native_identity_unproven')
    history = proof.get('history')
    if not isinstance(history, dict) or not isinstance(history.get('target_message'), dict):
        raise ValueError('crash_witness_native_identity_unproven')
    target = history['target_message']
    if (proof.get('exact_root_project_binding') is not True
            or proof.get('session') != binding['session']
            or proof.get('target_message_id') != 'msg_' + binding['delivery_id']
            or proof.get('history', {}).get('target_id_occurrences') != 1
            or target.get('status') != 'observed'
            or target.get('text_sha256') != binding['payload_sha256']):
        raise ValueError('crash_witness_native_identity_unproven')


def crash_message_content_facts(message, text_extract):
    content = message.get('content') if isinstance(message, dict) else None
    kind = ('string' if isinstance(content, str) else 'list' if isinstance(content, list)
            else 'null' if content is None else 'other')
    counts = {name: 0 for name in ('text', 'image_url', 'input_text', 'output_text',
                                   'tool_use', 'tool_result', 'other')}
    if isinstance(content, list):
        for block in content[:32]:
            name = block.get('type') if isinstance(block, dict) else None
            counts[name if isinstance(name, str) and name in counts else 'other'] += 1
    facts = {'type': kind, 'sha256': json_digest(content), 'block_types': counts,
             'blocks_truncated': isinstance(content, list) and len(content) > 32,
             'text_status': 'unsupported'}
    try:
        extracted = text_extract(content)
        if isinstance(extracted, str):
            facts.update(text_status='observed', text_sha256=hashlib.sha256(extracted.encode()).hexdigest(),
                         text_utf8_bytes=len(extracted.encode()))
    except Exception:
        pass  # Never export arbitrary parser errors or let diagnostics mask failure.
    return facts


def crash_suffix_mismatch_facts(messages, before, text_extract):
    roles = ('system', 'developer', 'user', 'assistant', 'tool')
    def role(message):
        value = message.get('role') if isinstance(message, dict) else None
        return value if isinstance(value, str) and value in roles else 'other'
    actual = messages if isinstance(messages, list) else []
    prefix = actual[:-1]
    systems = lambda values: [item for item in values if role(item) in ('system', 'developer')]
    old_system, new_system = systems(before), systems(prefix)
    facts = {'status': 'observed', 'expected_message_count': len(before) + 1,
             'actual_message_count': len(actual), 'actual_messages_list': isinstance(messages, list),
             'last_role': role(actual[-1]) if actual else 'other',
             'penultimate_role': role(actual[-2]) if len(actual) > 1 else 'other',
             'expected_prefix_sha256': json_digest(before), 'actual_prefix_sha256': json_digest(prefix),
             'system_equal': old_system == new_system,
             'expected_system_sha256': json_digest(old_system), 'actual_system_sha256': json_digest(new_system),
             'notification_message_equal': bool(before and len(actual) > 1 and before[-1] == actual[-2])}
    if before and len(actual) > 1:
        old, new = before[-1], actual[-2]
        left, right = crash_message_content_facts(old, text_extract), crash_message_content_facts(new, text_extract)
        facts['notification'] = {'expected_sha256': json_digest(old), 'actual_sha256': json_digest(new),
                                 'expected_content': left, 'actual_content': right,
                                 'text_equal': (left['text_sha256'] == right['text_sha256']
                                                if left['text_status'] == right['text_status'] == 'observed' else None)}
    for index in range(max(len(before), len(prefix))):
        old = before[index] if index < len(before) else None
        new = prefix[index] if index < len(prefix) else None
        if old == new:
            continue
        fields = ('role', 'content', 'tool_calls', 'tool_call_id', 'name', 'reasoning_content', 'providerOptions')
        old_map, new_map = old if isinstance(old, dict) else {}, new if isinstance(new, dict) else {}
        left, right = crash_message_content_facts(old, text_extract), crash_message_content_facts(new, text_extract)
        facts['first_mismatch'] = {
            'index': index, 'expected_role': role(old), 'actual_role': role(new),
            'expected_message_present': index < len(before), 'actual_message_present': index < len(prefix),
            'expected_sha256': json_digest(old), 'actual_sha256': json_digest(new),
            'changed_fields': [name for name in fields if (name in old_map) != (name in new_map)
                               or old_map.get(name) != new_map.get(name)],
            'other_fields_changed': ({key: value for key, value in old_map.items() if key not in fields}
                                     != {key: value for key, value in new_map.items() if key not in fields}),
            'expected_content': left, 'actual_content': right,
            'text_equal': (left['text_sha256'] == right['text_sha256']
                           if left['text_status'] == right['text_status'] == 'observed' else None)}
        break
    return facts


def owned_instruction_metadata(value, binding):
    if (not isinstance(value, dict) or value.get('exact_session_binding') is not True
            or value.get('exact_instruction_binding') is not True
            or value.get('session') != binding['session'] or not isinstance(value.get('text'), str)
            or hashlib.sha256(value['text'].encode()).hexdigest() != value.get('sha256')):
        raise ValueError('crash_witness_owned_instruction_unproven')
    root, path = value.get('instruction_root'), value.get('instruction_path')
    if (not isinstance(root, str) or not isinstance(path, str)
            or not root.startswith('/') or not path.startswith('/')
            or str(PurePosixPath(root)) != root or str(PurePosixPath(path)) != path
            or '..' in PurePosixPath(path).parts or '..' in PurePosixPath(root).parts
            or PurePosixPath(path).parent != PurePosixPath(root) / 'hrx'
            or not re.fullmatch(r'yesod-instructions-[0-9]+\.md', PurePosixPath(path).name)):
        raise ValueError('crash_witness_owned_instruction_unproven')
    return value


def exact_owned_instruction_append(previous, current, binding):
    # OpenCode v2.1.2 a517687f: instruction-discovery.ts renderUpdate;
    # ai/protocols/shared.ts wrapSystemUpdate; openai-chat.ts string-user merge.
    old, new = owned_instruction_metadata(previous, binding), owned_instruction_metadata(current, binding)
    if (old['instruction_root'] != new['instruction_root']
            or old['instruction_path'] == new['instruction_path']
            or old['text'] != new['text'] or old['sha256'] != new['sha256']):
        raise ValueError('crash_witness_owned_instruction_changed')
    update = ('The instructions from ' + old['instruction_path'] + ' no longer apply.\n\n'
              'New instructions apply from:\nInstructions from: ' + new['instruction_path'] + '\n' + new['text'])
    escaped = update.replace('&', '&amp;').replace('<', '&lt;').replace('>', '&gt;')
    return '\n<system-update>\n' + escaped + '\n</system-update>'


def retained_crash_suffix(request, witness, binding, proof, *, diagnostics=None, text_extract=None, initialization=None):
    if witness.get('authorized') is not True or witness.get('consumed') is True:
        raise ValueError('crash_witness_not_authorized_or_consumed')
    if binding != witness['binding']:
        raise ValueError('crash_witness_binding_changed')
    verify_crash_native_proof(proof, binding)
    messages = request.get('messages')
    before = witness['request']['messages']
    shape = (isinstance(messages, list) and len(messages) == len(before) + 1 and len(messages) >= 2
             and isinstance(messages[-2], dict) and isinstance(messages[-1], dict)
             and messages[-2].get('role') == 'user' and messages[-1].get('role') == 'user')
    forwarded = messages[:-1] if shape else None
    transform = None
    if shape and forwarded != before:
        try:
            append = exact_owned_instruction_append(witness.get('instruction'), initialization, binding)
            original, actual = before[-1], messages[-2]
            content = original.get('content')
            if (isinstance(content, str) and messages[:-2] == before[:-1]
                    and {key: value for key, value in original.items() if key != 'content'}
                        == {key: value for key, value in actual.items() if key != 'content'}
                    and actual.get('content') == content + append
                    and messages[-1].get('content') == initialization['text']):
                # Extract only after the COMPLETE source-equivalent transform
                # matched, by the witnessed original's exact prefix length.
                extracted = actual['content'][:len(content)]
                forwarded = [*messages[:-2], {**actual, 'content': extracted}]
                transform = {'kind': 'owned_instruction_removed_added',
                             'append_sha256': hashlib.sha256(append.encode()).hexdigest(),
                             'append_utf8_bytes': len(append.encode()),
                             'old_path_sha256': hashlib.sha256(witness['instruction']['instruction_path'].encode()).hexdigest(),
                             'new_path_sha256': hashlib.sha256(initialization['instruction_path'].encode()).hexdigest(),
                             'complete_transform_verified': True}
        except ValueError:
            pass  # The existing full-equality rejection/diagnostic remains authoritative.
    if not shape or forwarded != before:
        if diagnostics is not None and len(diagnostics) < 4:
            try:
                diagnostics.append(crash_suffix_mismatch_facts(messages, before, text_extract))
            except Exception:
                diagnostics.append({'status': 'unavailable'})
        raise ValueError('crash_witness_actual_suffix_changed')
    witness['consumed'] = True  # Before engine work; never authorize a second use.
    if transform is not None:
        witness['verified_transform'] = transform
    return {**request, 'messages': forwarded}


def validate_claude_generation(config, owner, session, previous=None):
    agents = config.get('agents')
    if (config.get('backend') != 'claude-task' or not isinstance(agents, list) or len(agents) != 1
            or not isinstance(agents[0], dict)
            or agents[0].get('agent_id') != owner or agents[0].get('thread_id') != session
            or agents[0].get('combined') is not True
            or not isinstance(config.get('state_directory'), str)
            or not config['state_directory']
            or not isinstance(config.get('claude_task_generation'), str)
            or not re.fullmatch('[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}',
                                config.get('claude_task_generation', ''))):
        raise ValueError('claude_combined_generation_binding_mismatch')
    if previous is not None and (
            config['claude_task_generation'] == previous['claude_task_generation']
            or config['state_directory'] != previous['state_directory']
            or config.get('claude_home') != previous.get('claude_home')):
        raise ValueError('claude_combined_successor_generation_mismatch')


class BackendProvider:
    def __init__(self, cwd):
        self.module = source_module('compact_opencode_adapter')
        self.engine = self.module.OpenCodeCompactAdapter(str(cwd), max_requests=256,
                                                        body_byte_limit=256, reply_to_asks=True)
        self.engine.expectation_loader = self.resolve
        self.counter = source_module('native_quiescence').ProviderCounter()
        self.config = None
        self.expected, self.records, self.seen = {}, [], []
        self.whole_source, self.errors, self.tool_diagnostics = [], [], []
        self.busy, self.busy_seen = threading.Event(), threading.Event()
        self.busy.set()
        self.parts, self.active_record = [], None
        self.completed = []
        self.ack_retry_authorizer = None
        self.server = None
        self.native_fault = lambda: None
        self.initialization_loader = lambda: None
        self.initialization_records = []
        self.initialization_session = None
        self.task_diagnostics = []
        self.attention_release, self.attention_seen = threading.Event(), threading.Event()
        self.attention_release.set()
        self.cancel_attention = False
        self.crash_capture_requested = False
        self.crash_witness = None
        self.crash_witness_records = []
        self.crash_witness_diagnostics = []
        self.crash_resume_proof = lambda: None
        self.request_diagnostics = []
        self.request_count = 0
        self._request_lock = threading.Lock()

    def record_open_code_request(self, request):
        expected = (self.active_record or {}).get('last_provider_reply', {}).get('call_sha256')
        facts = native_request_facts(request, expected)
        with self._request_lock:
            self.request_count += 1
            facts['sequence'] = self.request_count
            self.request_diagnostics.append(facts)
            del self.request_diagnostics[:-128]
        return facts

    def close(self):
        self.busy.set()
        self.cancel_attention = True
        self.attention_release.set()
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()
            self.server_thread.join(5)

    def open_code_server(self):
        provider = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                provider.counter.begin()
                recorded = False
                try:
                    size = int(self.headers.get('Content-Length', '0'))
                    if not 0 < size <= 1000000:
                        raise ValueError('bounded_request_required')
                    request = json.loads(self.rfile.read(size))
                    request_facts = provider.record_open_code_request(request)
                    recorded = True
                    initializations_before = len(provider.initialization_records)
                    reply = provider.respond(request) if request.get('tools') else 'Fixture metadata.'
                    if len(provider.initialization_records) != initializations_before:
                        request_facts['routing_class'] = (
                            'owned_initialization_with_tool_result'
                            if provider.initialization_records[-1].get('pending_result_consumed') is True
                            else 'owned_initialization')
                    delivery = native_reply_delivery(reply, provider.engine.responder.operation)
                    if delivery['kind'] == 'tool' and provider.active_record is not None:
                        provider.active_record['last_provider_reply'] = delivery
                        provider.active_record.setdefault('provider_reply_deliveries', []).append(delivery)
                        del provider.active_record['provider_reply_deliveries'][:-64]
                    delta = reply if isinstance(reply, dict) else {'role': 'assistant', 'content': reply}
                    delivery['response_streaming'] = request.get('stream') is True
                    delivery['finish_reason'] = 'tool_calls' if delta.get('tool_calls') else 'stop'
                    if request.get('stream'):
                        events = []
                        for block, stop in ((delta, None), ({}, 'tool_calls' if delta.get('tool_calls') else 'stop')):
                            events.append({'id': 'fixture', 'object': 'chat.completion.chunk', 'created': 1,
                                           'model': 'fixture', 'choices': [{'index': 0, 'delta': block, 'finish_reason': stop}]})
                        body = (''.join('data: ' + json.dumps(value) + '\n\n' for value in events)
                                + 'data: [DONE]\n\n').encode()
                        kind = 'text/event-stream'
                    else:
                        body = json.dumps({'id': 'fixture', 'object': 'chat.completion', 'created': 1,
                                           'model': 'fixture', 'choices': [{'index': 0, 'message': delta,
                                                                          'finish_reason': 'tool_calls' if delta.get('tool_calls') else 'stop'}],
                                           'usage': {'prompt_tokens': 1, 'completion_tokens': 1, 'total_tokens': 2}}).encode()
                        kind = 'application/json'
                    self.send_response(200)
                    self.send_header('Content-Type', kind)
                    self.send_header('Content-Length', str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                    delivery['http_write_returned'] = True
                except OwnedGateCancellation:
                    self.close_connection = True
                except (BrokenPipeError, ConnectionResetError):
                    # An owned stop can cancel the explicitly held foreground
                    # turn. No source completion or ACK is synthesized.
                    if not provider.busy_seen.is_set():
                        provider.errors.append('unexpected_provider_transport_loss')
                except Exception as error:
                    code = str(error)
                    provider.errors.append(code if re.fullmatch('[a-z0-9_]{1,80}', code) else type(error).__name__)
                    self.send_response(500)
                    self.end_headers()
                finally:
                    if not recorded:
                        provider.record_open_code_request(None)
                    provider.counter.end()

        self.server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.server_thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.server_thread.start()
        return f'http://127.0.0.1:{self.server.server_port}'

    def crash_binding(self):
        from ysg_mail import codex_bridge as bridge
        from ysg_mail.attention_protocol import Metadata, Receipt, _binding
        config = bridge._load_config(self.config)
        if config.backend != 'opencode':
            raise ValueError('crash_witness_backend_mismatch')
        store = bridge.Store(config, config.agents[0])
        metadata = Metadata.model_validate_json((store.directory / 'attention-metadata.json').read_bytes())
        receipt = Receipt.model_validate_json((store.directory / 'attention-presentation.json').read_bytes())
        with store.access() as (_, state):
            pending = state['pending']
            if (pending is None or pending['ack_requested']
                    or any(row['event_key'] == receipt.delivery_id for row in state['handled'])
                    or pending['event'] != metadata.event):
                raise ValueError('crash_witness_source_not_pending')
        event = bridge._event(metadata.event, store.agent)
        bridge._validate_binding(store, event, metadata.origin)
        if receipt.binding != _binding(store, event, metadata.origin):
            raise ValueError('crash_witness_receipt_binding_changed')
        return {'owner': store.agent.agent_id, 'session': store.agent.thread_id, 'delivery_id': receipt.delivery_id,
                'receipt_binding': receipt.binding, 'epoch': receipt.epoch,
                'source_envelope_sha256': json_digest(event),
                'payload_sha256': hashlib.sha256(receipt.presentation.text.encode()).hexdigest()}

    def capture_crash_candidate(self, request):
        if self.crash_witness is not None:
            raise ValueError('crash_witness_duplicate_candidate')
        binding = self.crash_binding()
        messages = request.get('messages')
        if (not isinstance(messages, list) or not messages or not isinstance(messages[-1], dict)
                or messages[-1].get('role') != 'user'
                or hashlib.sha256(self.module.core.CompactResponder._text(
                    messages[-1].get('content')).encode()).hexdigest() != binding['payload_sha256']):
            raise ValueError('crash_witness_actual_notification_mismatch')
        instruction = owned_instruction_metadata(self.initialization_loader(), binding)
        self.crash_witness = {'binding': binding, 'instruction': json.loads(json.dumps(instruction)),
                              'request': json.loads(json.dumps(request)),
                              'authorized': False, 'consumed': False}

    def authorize_crash_witness(self, boundary, stopped):
        witness = self.crash_witness
        if (witness is None or witness['authorized'] or boundary.get('phase') != 'after-admission'
                or boundary.get('native_submit_returned') is not True
                or boundary.get('owner') != witness['binding']['owner']
                or boundary.get('session') != witness['binding']['session']
                or boundary.get('delivery_id') != witness['binding']['delivery_id']
                or stopped.get('native_stop_verified') is not True
                or stopped.get('transport_healthy') is not False
                or stopped.get('controller_healthy') is not False
                or self.crash_binding() != witness['binding']):
            raise ValueError('crash_witness_stop_boundary_unproven')
        before = stopped.get('native_lifecycle', [])
        if len(before) != 1 or before[0].get('stage') != 'before_stop':
            raise ValueError('crash_witness_stop_history_missing')
        verify_crash_native_proof(before[0], witness['binding'])
        witness['authorized'] = True
        self.crash_capture_requested = False

    def arm(self, key, label, snapshot):
        if key in self.expected:
            raise ValueError('fixture_duplicate_source_key')
        self.expected[key] = {'label': label, 'snapshot': snapshot}
        self.whole_source.append(snapshot.body.encode())

    def resolve(self, text):
        from ysg_mail import codex_bridge as bridge
        from ysg_mail.attention_protocol import Metadata, Receipt

        config = bridge._load_config(self.config)
        store = bridge.Store(config, config.agents[0])
        metadata = Metadata.model_validate_json((store.directory / 'attention-metadata.json').read_bytes())
        receipt = Receipt.model_validate_json((store.directory / 'attention-presentation.json').read_bytes())
        event = bridge._event(metadata.event, store.agent)
        if text != receipt.presentation.text or receipt.header.combined_delivery_id != event['event_key']:
            raise ValueError('exact_combined_notice_required')
        bridge._validate_binding(store, event, metadata.origin)
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
            snapshot = snapshot.model_copy(update={'event_key': notice['notice_id'], 'sender': notice['actor']})
            expected['snapshot'] = snapshot
        elif (snapshot.sequence != delivery['attention']['inbox_seq' if delivery['source'] == 'dm' else 'seq']
              or snapshot.event_key != delivery['attention']['event_key']):
            raise ValueError('independent_source_binding_changed')
        if any(record['source_key'] == key for record in self.records):
            raise ValueError('duplicate_native_handling_effect')
        self.engine.responder.reply_to_asks = (
            snapshot.source == 'ask' and json.loads(snapshot.body)['state']['status'] == 'open')
        self.active_record = {
            'label': expected['label'], 'source_key': key, 'source': snapshot.source,
            'source_event_key': snapshot.event_key, 'session': store.agent.thread_id,
            'delivery_id': delivery['delivery_id'], 'source_monitor': delivery['source_monitor'],
            'selection': delivery.get('notice') or delivery['attention'],
            'bridge_delivery_id': receipt.delivery_id, 'bridge_event_sha256': json_digest(event),
            'selected_envelope_sha256': json_digest(delivery),
            'notification_utf8_bytes': len(text.encode()), 'payload_sha256': hashlib.sha256(text.encode()).hexdigest(),
            'expected_read_sha256': hashlib.sha256(snapshot.body.encode()).hexdigest(),
            'read_json_utf8_bytes': 0, 'completed': False,
        }
        from ysg_mail.native_publication import observed_attempt
        self.active_record['native_publication_observation'] = observed_attempt(store, receipt)
        self.records.append(self.active_record)
        self.seen.append(receipt)
        return receipt, snapshot

    def respond(self, request):
        last = request['messages'][-1]
        if last.get('role') == 'user':
            initialization = self.initialization_loader()
            text = self.module.core.CompactResponder._text(last.get('content'))
            if initialization is not None and text == initialization['text']:
                digest = hashlib.sha256(text.encode()).hexdigest()
                if (digest != initialization['sha256'] or initialization.get('exact_session_binding') is not True
                        or not re.fullmatch('ses[A-Za-z0-9_-]{1,252}', initialization.get('session', ''))
                        or self.initialization_session is not None and initialization['session'] != self.initialization_session):
                    raise ValueError('owned_initialization_binding_mismatch')
                self.initialization_session = initialization['session']
                recorded = {'session': initialization['session'], 'primer_sha256': digest,
                            'utf8_bytes': len(text.encode()), 'pending_result_consumed': False}
                self.initialization_records.append(recorded)
                responder = self.engine.responder
                if responder.call_id is not None and not responder.completed:
                    pending = responder.call_id
                    forwarded = owned_initialization_tool_suffix(request, pending)
                    reply = self.respond(forwarded)  # Same actual result, same strict validation.
                    recorded.update(pending_result_consumed=True,
                                    pending_call_sha256=hashlib.sha256(pending.encode()).hexdigest())
                    return reply
                witness = getattr(self, 'crash_witness', None)
                if witness is not None and witness.get('authorized') and not witness.get('consumed'):
                    try:
                        native_proof = self.crash_resume_proof()
                    except (OSError, ValueError):
                        raise ValueError('crash_witness_native_identity_unproven') from None
                    forwarded = retained_crash_suffix(
                        request, witness, self.crash_binding(), native_proof,
                        diagnostics=self.crash_witness_diagnostics,
                        text_extract=self.module.core.CompactResponder._text, initialization=initialization)
                    self.crash_witness_records.append({**witness['binding'],
                        'actual_request_prefix_sha256': json_digest(forwarded['messages']),
                        'native_history_verified': True, 'native_stop_verified': True,
                        'consumed_once': True,
                        'instruction_transform': witness.get('verified_transform', {'kind': 'none'})})
                    recorded['retained_crash_notification_consumed'] = True
                    return self.respond(forwarded)
                return 'Owned HRX initialization received; waiting for authorized notifications.'
        if last.get('role') == 'user' and last.get('content') == 'fixture-authorized-busy-turn':
            self.busy_seen.set()
            if not self.busy.wait(60):
                raise ValueError('busy_fixture_release_deadline')
            return 'Authorized foreground fixture completed.'
        if last.get('role') == 'user' and not self.attention_release.is_set():
            if self.crash_capture_requested:
                self.capture_crash_candidate(request)
            self.attention_seen.set()
            if not self.attention_release.wait(60):
                raise ValueError('native_attention_pause_deadline')
            if self.cancel_attention:
                raise OwnedGateCancellation()
        responder = self.engine.responder
        result = operation = stdout = None
        if last.get('role') == 'tool':
            if last.get('tool_call_id') != responder.call_id:
                raise ValueError('uncorrelated_fixture_tool_result')
            content = responder._text(last['content'])
            if responder.operation == 'ack' and self.ack_retry_authorizer is not None:
                failed = self.module.envelope.inspect_result(content, responder.call_id)
                if failed['returncode'] != 0:
                    phase = self.ack_retry_authorizer(responder.expected.delivery_id)
                    if phase not in {'before-ack-commit', 'after-ack-commit', 'lost-ack-response'}:
                        raise ValueError('fixture_ack_fault_authorization_refused')
                    self.active_record.setdefault('ack_recovery_events', []).append({
                        'phase': phase, 'failed_returncode': failed['returncode'], 'ack_receipt_observed': False})
                    return self.engine._wrap(responder.retry_pending_ack(responder.call_id))
            framed = self.module.envelope.decode(content, responder.call_id)
            stdout = framed['stdout']
            result, operation = json.loads(stdout), responder.operation
        reply = self.engine.respond(request)
        if result is not None:
            if operation == 'read':
                self.parts.append(result['body_utf8'].encode())
                self.active_record['read_json_utf8_bytes'] += len(stdout.encode())
            elif operation == 'reply':
                self.active_record['recipient_reply_version'] = result['version']
            elif operation == 'ack':
                self.active_record['helper_ack'] = result
        if isinstance(reply, dict) and responder.operation == 'ack':
            self.exact_bytes()
        if isinstance(reply, str) and self.engine.facts()['completed']:
            self.exact_bytes()
            self.active_record.update(completed=True, post_ack_continuation=True)
            self.completed.append(self.active_record['source'])
            self.parts = []
            reply += '\nfixture-combined-complete:' + self.active_record['source']
        return reply

    def exact_bytes(self):
        actual = b''.join(self.parts)
        expected = self.expected[self.active_record['source_key']]['snapshot'].body.encode()
        if actual != expected:
            raise ValueError('independent_exact_read_bytes_mismatch')
        self.active_record.update(read_sha256=hashlib.sha256(actual).hexdigest(), read_body_utf8_bytes=len(actual))


class ClaudeTaskScript:
    """Use the real Claude task ID/output handshake, then common exact tools."""

    def __init__(self, provider, base, config_path):
        self.provider, self.base, self.config_path = provider, base, config_path
        self.task = None
        self.retired_tasks = set()
        self.fresh_generation_allowed = False
        self.pending = None
        self.sequence = 0
        self.violations = provider.errors
        self.lock = threading.Lock()

    def retire_current_task(self):
        # Publish retirement before signaling the native tree: the background
        # task can notify Claude while the supervisor is still shutting down.
        if self.task is not None:
            self.retired_tasks.add(self.task[0])
        self.fresh_generation_allowed = False

    def violate(self, code):
        self.violations.append(code)

    def tool(self, purpose, command, *, background=False, tool_id=None):
        self.sequence += 1
        identifier = tool_id or f'toolu_combined_{self.sequence}'
        self.pending = (identifier, purpose)
        value = {'command': command, 'description': 'Combined acceptance ' + purpose}
        if background:
            value['run_in_background'] = True
        return {'tool': {'id': identifier, 'name': 'Bash', 'input': value}}

    def arm(self, system=None):
        config, owner, session = self.config_path(system)
        self.fresh_generation_allowed = False
        command = shlex.join(['pitput', 'bridge', 'claude-task', '--config', str(config),
                              '--agent-id', owner, '--session-id', session])
        return self.tool('arm', 'umask 077\nwhile :; do\n event=$(' + command + ') || exit "$?"\n'
                         ' if [ -n "$event" ]; then printf \'%s\\n\' "$event"; break; fi\ndone', background=True)

    def decide(self, request):
        with self.lock:
            self.provider.counter.begin()
            try:
                return self._decide(request)
            except OwnedGateCancellation:
                return {'text': 'Owned native request canceled at the injected boundary.'}
            except Exception as error:
                code = str(error)
                self.violate(code if re.fullmatch('[a-z0-9_]{1,80}', code) else type(error).__name__)
                return {'text': 'Fixture stopped.'}
            finally:
                self.provider.counter.end()

    def _decide(self, request):
        if not any(tool.get('name') == 'Bash' for tool in request.get('tools') or []):
            return {'text': 'ok'}
        last = self.base.latest_dialogue_message(request)
        content = last.get('content')
        text = self.base.text_of(content)
        if text == 'fixture-authorized-busy-turn':
            reply = self.provider.respond({'messages': [{'role': 'user', 'content': text}]})
            return {'text': reply}
        if '<task-notification>' in text:
            task_id = self.base.tag(text, 'task-id')
            output = self.base.tag(text, 'output-file')
            status = self.base.tag(text, 'status')
            retired = task_id in self.retired_tasks
            self.provider.task_diagnostics.append({
                'task_id_present': bool(task_id), 'output_file_present': bool(output),
                'task_matches_current': self.task is not None and task_id == self.task[0],
                'output_matches_current': self.task is not None and output == self.task[1],
                'known_retired_task': retired,
                'status': status if status in ('completed', 'failed', 'killed', 'running') else 'other',
            })
            del self.provider.task_diagnostics[:-64]
            if retired:
                if self.fresh_generation_allowed and self.pending is None:
                    # binding() must first verify the new generation in this
                    # actual native system. The old task output is never read.
                    return self.arm(self.base.text_of(request.get('system')))
                return {'text': 'Predecessor task retired; awaiting fresh session commands.'}
        blocks = content if isinstance(content, list) else []
        results = [block for block in blocks if isinstance(block, dict) and block.get('type') == 'tool_result']
        if self.pending is not None:
            identifier, purpose = self.pending
            if len(results) != 1 or results[0].get('tool_use_id') != identifier or results[0].get('is_error'):
                diagnostic = {'purpose': purpose if purpose in ('arm', 'event', 'native-tool') else 'other',
                              'result_count': min(len(results), 100),
                              'expected_id_sha256': hashlib.sha256(identifier.encode()).hexdigest(),
                              'id_matches': len(results) == 1 and results[0].get('tool_use_id') == identifier,
                              'native_error': len(results) == 1 and results[0].get('is_error') is True}
                self.provider.tool_diagnostics.append(diagnostic)
                del self.provider.tool_diagnostics[:-64]
                if len(results) != 1:
                    raise ValueError('claude_task_tool_result_count')
                if results[0].get('tool_use_id') != identifier:
                    raise ValueError('claude_task_tool_result_id')
                raise ValueError('claude_task_tool_result_native_error')
            self.pending = None
            output = self.base.text_of(results[0].get('content')).removesuffix('\n\n[exited with code 0]')
            if purpose == 'arm':
                found = re.search(r'\bID:\s*([A-Za-z0-9_-]+).*?written to:\s*(\S+?)\.?\s*(?:$|You )', output, re.S)
                if not found:
                    raise ValueError('background_result_unrecognized')
                self.task = (found.group(1), found.group(2))
                return {'text': 'Waiting for the next Pitput event.'}
            message = {'role': 'user', 'content': output} if purpose == 'event' else {
                'role': 'tool', 'content': output, 'tool_call_id': identifier}
            reply = self.provider.respond({'messages': [message], 'tools': [
                {'type': 'function', 'function': {'name': 'shell'}}]})
            if isinstance(reply, str):
                return self.arm()
            call = reply['tool_calls'][0]
            args = json.loads(call['function']['arguments'])
            return self.tool('native-tool', 'cd ' + shlex.quote(args['workdir']) + ' && ' + args['command'],
                             tool_id=call['id'])
        if '<task-notification>' in text:
            fault = self.provider.native_fault()
            if (self.task is not None and self.base.tag(text, 'task-id') == self.task[0]
                    and self.base.tag(text, 'output-file') == self.task[1]
                    and self.base.tag(text, 'status') == 'failed'
                    and fault is not None and fault.get('phase') == 'before-admission'
                    and fault.get('session') == self.config_path()[2]
                    and fault.get('owner') == self.config_path()[1]):
                self.task = None
                return {'text': 'Owned task stopped at the injected before-admission boundary.'}
            if (self.task is None or self.base.tag(text, 'task-id') != self.task[0]
                    or self.base.tag(text, 'output-file') != self.task[1]
                    or self.base.tag(text, 'status') != 'completed'):
                raise ValueError('claude_foreign_or_failed_task_notification')
            return self.tool('event', 'cat ' + shlex.quote(self.task[1]))
        if self.task is not None:
            raise ValueError('claude_unexpected_foreground_request')
        return self.arm(self.base.text_of(request.get('system')))
