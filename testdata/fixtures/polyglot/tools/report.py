from dataclasses import dataclass


@dataclass
class Line:
    description: str
    amount: int


def subtotal(lines: list[Line]) -> int:
    return sum(line.amount for line in lines)


class Report:
    def __init__(self, lines: list[Line]):
        self.lines = lines

    def render(self) -> str:
        return f"{len(self.lines)} lines, subtotal {subtotal(self.lines)}"
