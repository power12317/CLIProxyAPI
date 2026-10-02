import unittest

from server import parse_request


class RequestContractTest(unittest.TestCase):
    def test_plain_text_defaults_to_medium(self):
        prompt, stream = parse_request({"model": "gpt-5.6-sol", "input": "hello"})
        self.assertIn("[user]", prompt)
        self.assertFalse(stream)

    def test_rejects_tools(self):
        with self.assertRaisesRegex(Exception, "tools"):
            parse_request({"model": "gpt-5.6-sol", "input": "hello", "tools": []})

    def test_rejects_other_model(self):
        with self.assertRaisesRegex(Exception, "gpt-5.6-sol"):
            parse_request({"model": "gpt-6.1-sol", "input": "hello"})


if __name__ == "__main__":
    unittest.main()
