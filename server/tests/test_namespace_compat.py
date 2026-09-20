import json
import unittest

from server.routers import responses as R


def ns_tools():
    return [
        {'type': 'function', 'name': 'exec_command',
         'parameters': {'type': 'object', 'properties': {'cmd': {'type': 'string'}}}},
        {'type': 'namespace', 'name': 'shell', 'tools': [
            {'type': 'custom', 'name': 'apply_patch'},
            {'type': 'function', 'name': 'read_file',
             'parameters': {'type': 'object', 'properties': {'path': {'type': 'string'}}}},
        ]},
        {'type': 'namespace', 'name': 'nested', 'tools': [
            {'type': 'namespace', 'name': 'inner', 'tools': [
                {'type': 'function', 'name': 'stat',
                 'parameters': {'type': 'object', 'properties': {'path': {'type': 'string'}}}},
            ]},
        ]},
        {'type': 'function', 'name': 'read_file',
         'parameters': {'type': 'object', 'properties': {'path': {'type': 'string'}}}},
    ]


class NamespaceCompatTests(unittest.TestCase):
    def test_children_are_flattened_and_conflicts_renamed(self):
        bridge = R._ToolBridge(ns_tools())
        names = [t['function']['name'] for t in bridge.chat_tools]
        self.assertEqual(names, ['exec_command', 'apply_patch', 'read_file', 'stat', 'read_file_2'])
        self.assertIn('apply_patch', bridge.custom_names)
        self.assertEqual(bridge.outbound_name('apply_patch'), 'apply_patch')
        self.assertEqual(bridge.outbound_name('read_file'), 'read_file')
        self.assertEqual(bridge.restore('read_file_2'), ('read_file', 'function'))
        self.assertEqual(bridge.restore('stat'), ('stat', 'function'))

    def test_history_and_choice_use_outbound_names(self):
        body = {
            'model': 'm',
            'tools': ns_tools(),
            'tool_choice': {'type': 'custom', 'name': 'apply_patch'},
            'input': [
                {'type': 'custom_tool_call', 'call_id': 'c1', 'name': 'apply_patch', 'input': 'patch'},
                {'type': 'custom_tool_call_output', 'call_id': 'c1', 'output': 'ok'},
                {'type': 'function_call', 'call_id': 'c2', 'name': 'read_file',
                 'arguments': '{"path":"a"}'},
                {'type': 'function_call_output', 'call_id': 'c2', 'output': 'a'},
            ],
        }
        chat = R.to_chat_request(body)
        self.assertEqual(chat['tool_choice'], {'type': 'function', 'function': {'name': 'apply_patch'}})
        names = [m['tool_calls'][0]['function']['name'] for m in chat['messages'] if m.get('tool_calls')]
        self.assertEqual(names, ['apply_patch', 'read_file'])
        self.assertEqual(json.loads(chat['messages'][0]['tool_calls'][0]['function']['arguments']),
                         {'input': 'patch'})

    def test_response_restores_original_child_names(self):
        bridge = R._ToolBridge(ns_tools())
        data = {'choices': [{'finish_reason': 'tool_calls', 'message': {
            'tool_calls': [
                {'id': '1', 'function': {'name': 'apply_patch', 'arguments': '{"input":"patch"}'}},
                {'id': '2', 'function': {'name': 'read_file_2', 'arguments': '{"path":"a"}'}},
            ]
        }}]}
        out = R.to_responses_object(data, 'm', 'resp_1', bridge.custom_names, bridge)['output']
        self.assertEqual([(i['type'], i['name']) for i in out],
                         [('custom_tool_call', 'apply_patch'), ('function_call', 'read_file')])
        self.assertEqual(out[0]['input'], 'patch')

    def test_stream_restores_custom_child(self):
        bridge = R._ToolBridge(ns_tools())
        t = R._StreamTranslator('m', 'resp_1', bridge.custom_names, bridge)
        events = t.feed({'choices': [{'delta': {'tool_calls': [
            {'index': 0, 'id': 'call', 'function': {'name': 'apply_patch', 'arguments': '{"input":"p"}'}}
        ]}, 'finish_reason': 'tool_calls'}]})
        events += t.finish('tool_calls')
        items = [e for e in events if e.startswith(b'event: response.output_item.done')]
        self.assertTrue(items)
        payload = json.loads(items[-1].split(b'\ndata: ', 1)[1].split(b'\n\n', 1)[0])
        self.assertEqual(payload['item']['type'], 'custom_tool_call')
        self.assertEqual(payload['item']['name'], 'apply_patch')
        self.assertEqual(payload['item']['input'], 'p')


if __name__ == '__main__':
    unittest.main()
