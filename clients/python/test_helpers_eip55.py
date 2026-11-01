import io, unittest
import swarmmemo

ADDR = "0xD7a137933471C895B05C7413285Be5815D5A9466"


class PayoutChecksumNote(unittest.TestCase):
    def note(self, address):
        err = io.StringIO()
        swarmmemo.note_payout_checksum(address, err)
        return err.getvalue()

    def test_checksum_roundtrip(self):
        self.assertEqual(swarmmemo.evm_checksum(ADDR.lower()), ADDR)
        self.assertEqual(swarmmemo.evm_checksum("0x" + ADDR[2:].upper()), ADDR)

    def test_lowercase_prints_eip55(self):
        self.assertEqual(self.note(ADDR.lower()), f"payout address (EIP-55): {ADDR}\n")

    def test_checksummed_or_absent_or_invalid_is_silent(self):
        for a in (ADDR, None, "", "0x" + "0" * 40, "0xabc"):
            self.assertEqual(self.note(a), "")


if __name__ == "__main__":
    unittest.main()
